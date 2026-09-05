package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shutdowncheck/shutdowncheck/internal/analyze"
	"github.com/shutdowncheck/shutdowncheck/internal/timeline"
	"github.com/shutdowncheck/shutdowncheck/pkg/schema"
)

func TestAnalyzeRestoresRecordedPolicyAndTrialSummary(t *testing.T) {
	policy, err := analyze.PolicyFor(analyze.ProfileStandalone)
	if err != nil {
		t.Fatalf("PolicyFor: %v", err)
	}
	minimumScore := 100
	policy.MinScore = &minimumScore

	recorder := timeline.NewRecorder(timeline.Meta{
		ToolVersion: "recording-version", Profile: string(policy.Profile), Target: "pid(42)",
	}, 100)
	for id := uint64(1); id <= 10; id++ {
		recorder.Record(timeline.RequestAt(timeline.RequestEvent{
			ID: id, Method: "GET", URL: "http://example.test/work",
			Sent: 4900 * time.Millisecond, Done: 5100 * time.Millisecond,
			Status: 200, Outcome: timeline.OutcomeOK,
		}))
	}
	recorder.Record(timeline.RequestAt(timeline.RequestEvent{
		ID: 11, Method: "GET", URL: "http://example.test/work",
		Sent: time.Second, Done: 1200 * time.Millisecond,
		Status: 200, Outcome: timeline.OutcomeOK,
	}))
	recorder.Record(timeline.SignalAt(5*time.Second, timeline.SignalEvent{Signal: "TERM"}))
	recorder.Record(timeline.ConnectionAt(5200*time.Millisecond, timeline.ConnectionEvent{
		ID: 1, Phase: timeline.ConnReuse, Reused: true,
	}))
	exitCode := 0
	recorder.Record(timeline.ProcessAt(6*time.Second, timeline.ProcessEvent{
		Phase: timeline.ProcExited, PID: 42, ExitCode: &exitCode,
	}))

	tl := recorder.Snapshot()
	input := analyze.Input{
		Timeline: tl, Policy: policy, ToolVersion: "recording-version",
		Target: analyze.TargetInfo{Kind: analyze.TargetProcess, Label: "pid(42)", PID: 42},
		Probe:  analyze.ProbeInfo{URL: "http://example.test/work", Method: "GET"},
		Load: analyze.LoadInfo{
			Calibrated: true, RPS: 100, TargetInFlight: 10,
			BaselineLatency: 200 * time.Millisecond, GoalEvaluated: true, Achievable: true,
		},
	}
	tl.Meta.Analysis = encodeAnalysisContext(input)
	tl.Meta.Analysis.Trials = timeline.AnalysisTrials{Total: 3, Failed: 1, Consistent: false}

	path := filepath.Join(t.TempDir(), "run.ndjson")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create recording: %v", err)
	}
	if err := timeline.WriteNDJSON(file, tl); err != nil {
		_ = file.Close()
		t.Fatalf("write recording: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close recording: %v", err)
	}

	code, stdout, stderr := execute(t, "analyze", path, "--format", "json")
	if code != schema.ExitFail {
		t.Fatalf("default replay exit = %d, want fail\nstderr: %s", code, stderr)
	}
	var report schema.Report
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if report.Run.Trials.Total != 3 || report.Run.Trials.Failed != 1 || report.Run.Trials.Consistent {
		t.Errorf("trial summary = %+v, want 1 of 3 and inconsistent", report.Run.Trials)
	}
	if report.Target.Kind != string(analyze.TargetProcess) || report.Probe.URL != "http://example.test/work" {
		t.Errorf("recorded analysis metadata was not restored: target=%+v probe=%+v", report.Target, report.Probe)
	}
	if report.Load.BaselineLatencyMS.P50 != 200 {
		t.Errorf("baseline p50 = %vms, want 200ms", report.Load.BaselineLatencyMS.P50)
	}

	code, _, stderr = execute(t, "analyze", path, "--profile", "standalone", "--format", "json")
	if code != schema.ExitPass {
		t.Fatalf("explicit profile replay exit = %d, want pass after resetting custom gate\nstderr: %s", code, stderr)
	}
}

func TestDecodeAnalysisContextRejectsInvalidStoredPolicy(t *testing.T) {
	policy, err := analyze.PolicyFor(analyze.ProfileStandalone)
	if err != nil {
		t.Fatalf("PolicyFor: %v", err)
	}
	input := analyze.Input{
		Policy: policy,
		Target: analyze.TargetInfo{Kind: analyze.TargetProcess},
		Load:   analyze.LoadInfo{Achievable: true},
	}
	context := encodeAnalysisContext(input)
	context.Policy.ReadinessFlipBudget = -time.Second
	tl := timeline.Timeline{Meta: timeline.Meta{Analysis: context}}

	if _, _, err := decodeAnalysisContext(tl); err == nil {
		t.Fatal("invalid stored policy was accepted")
	}
}
