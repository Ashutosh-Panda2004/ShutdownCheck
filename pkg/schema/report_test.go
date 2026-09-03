package schema

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite golden files")

func ptrInt64(v int64) *int64 { return &v }
func ptrInt(v int) *int       { return &v }

// sampleReport mirrors the illustrative report in spec section 13, so drift
// between the specification and the code shows up as a failing test.
func sampleReport() Report {
	return Report{
		SchemaVersion: SchemaVersion,
		ToolVersion:   "1.0.0",
		Verdict:       VerdictFail,
		Score:         Score{Value: 34, Grade: Grade(34), WeightsVersion: WeightsVersion},
		Run: Run{
			StartedAt: time.Date(2026, time.September, 3, 10, 15, 18, 0, time.UTC),
			Profile:   "kubernetes",
			Seed:      42,
			Trials:    Trials{Total: 3, Failed: 3, Consistent: true},
		},
		Target: Target{
			Kind:          "command",
			Label:         "./bin/api --port 8080",
			PID:           12345,
			DetectedStack: "dotnet-aspnetcore",
			GracePeriodMS: 5000,
		},
		Probe: Probe{
			URL:          "http://localhost:8080/api/orders",
			Method:       "POST",
			ReadinessURL: "http://localhost:8080/readyz",
		},
		Load: Load{
			Calibrated:               true,
			RPS:                      340,
			TargetInFlight:           22,
			ObservedInFlightAtSignal: 22,
			BaselineLatencyMS:        Latency{P50: 62, P95: 88, P99: 104},
		},
		Timeline: Timeline{
			SignalSentAtMS:       0,
			ReadinessFlippedAtMS: nil,
			ListenerClosedAtMS:   ptrInt64(12),
			SigkillSentAtMS:      ptrInt64(5000),
			ProcessExitedAtMS:    ptrInt64(6400),
			PortReleasedAtMS:     ptrInt64(6410),
			ShutdownDurationMS:   ptrInt64(6400),
		},
		Requests: Requests{
			Total: 1364,
			ByPhase: Phases{
				Steady:     PhaseStats{Count: 1024, OK: 1024},
				InFlight:   PhaseStats{Count: 22, OK: 16, Failed: 6, Failures: map[string]int{"reset": 4, "timeout": 2}},
				PostSignal: PhaseStats{Count: 318, Failed: 318, Failures: map[string]int{"refused": 318}},
			},
			DrainLatencyMS: Latency{P50: 71, P95: 210, P99: 980},
		},
		Connections: Connections{
			Opened:            128,
			ReusedAfterSignal: 41,
			ClosedByFIN:       87,
			ClosedByRST:       41,
		},
		Process: Process{ExitCode: ptrInt(0), TerminatedBySignal: "SIGKILL"},
		Findings: []Finding{{
			ID:            "SC003",
			Name:          "IN_FLIGHT_DROPPED",
			Stage:         "S6",
			Severity:      SeverityError,
			Summary:       "6 of 22 in-flight requests were destroyed during shutdown.",
			Evidence:      map[string]any{"in_flight_total": 22, "in_flight_failed": 6},
			Impact:        "In-progress user requests fail on every deploy.",
			RemediationID: "SC003/dotnet-aspnetcore",
			DocsURL:       "https://shutdowncheck.dev/signatures/SC003",
		}},
		Gates: []Gate{{Name: "max_inflight_drop_pct", Threshold: 0, Actual: 27.3, Passed: false}},
	}
}

func TestReportMatchesGolden(t *testing.T) {
	got, err := json.MarshalIndent(sampleReport(), "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n')

	golden := filepath.Join("testdata", "report.golden.json")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(golden, got, 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}

	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run: go test ./pkg/schema -update): %v", err)
	}

	if string(got) != string(want) {
		t.Errorf("report JSON changed.\nThis is a public contract; if the change is intended, bump SchemaVersion,\n"+
			"write an ADR, and refresh with: go test ./pkg/schema -update\n\ngot:\n%s\n\nwant:\n%s", got, want)
	}
}

// The report is consumed by CI systems and compared across runs, so identical
// input must always serialise to identical bytes.
func TestMarshallingIsDeterministic(t *testing.T) {
	first, err := json.Marshal(sampleReport())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	for range 50 {
		next, err := json.Marshal(sampleReport())
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(next) != string(first) {
			t.Fatal("marshalling the same report produced different bytes")
		}
	}
}

func TestReportRoundTrips(t *testing.T) {
	want := sampleReport()

	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Report
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.Verdict != want.Verdict || got.Score != want.Score {
		t.Errorf("verdict/score did not round-trip: %+v", got)
	}
	if got.Timeline.ReadinessFlippedAtMS != nil {
		t.Error("a null measurement round-tripped as non-nil")
	}
	if got.Timeline.ListenerClosedAtMS == nil || *got.Timeline.ListenerClosedAtMS != 12 {
		t.Errorf("listener_closed_at_ms did not round-trip: %v", got.Timeline.ListenerClosedAtMS)
	}
	if len(got.Findings) != 1 || got.Findings[0].ID != "SC003" {
		t.Errorf("findings did not round-trip: %+v", got.Findings)
	}
}

// An absent measurement must serialise as null, never as 0, or a consumer
// cannot tell "flipped instantly" from "never flipped".
func TestAbsentMeasurementsSerialiseAsNull(t *testing.T) {
	data, err := json.Marshal(Timeline{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, field := range []string{
		"readiness_flipped_at_ms",
		"listener_closed_at_ms",
		"sigkill_sent_at_ms",
		"process_exited_at_ms",
		"port_released_at_ms",
		"shutdown_duration_ms",
	} {
		value, ok := raw[field]
		if !ok {
			t.Errorf("%s is missing; it must be present and null", field)
			continue
		}
		if value != nil {
			t.Errorf("%s = %v, want null", field, value)
		}
	}
}

func TestVerdictExitCodes(t *testing.T) {
	cases := map[Verdict]int{
		VerdictPass:         0,
		VerdictFail:         1,
		VerdictInconclusive: 2,
		Verdict("bogus"):    5,
	}
	for verdict, want := range cases {
		if got := verdict.ExitCode(); got != want {
			t.Errorf("%q.ExitCode() = %d, want %d", verdict, got, want)
		}
	}
}

func TestGradeBoundaries(t *testing.T) {
	cases := map[int]string{
		100: "A", 90: "A",
		89: "B", 80: "B",
		79: "C", 70: "C",
		69: "D", 60: "D",
		59: "F", 0: "F",
	}
	for score, want := range cases {
		if got := Grade(score); got != want {
			t.Errorf("Grade(%d) = %q, want %q", score, got, want)
		}
	}
}

func TestSchemaVersionIsSet(t *testing.T) {
	if SchemaVersion == "" || WeightsVersion == "" {
		t.Fatal("SchemaVersion and WeightsVersion must both be set")
	}
}
