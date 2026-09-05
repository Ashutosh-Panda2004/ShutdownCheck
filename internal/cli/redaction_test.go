package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shutdowncheck/shutdowncheck/internal/analyze"
	"github.com/shutdowncheck/shutdowncheck/internal/config"
	"github.com/shutdowncheck/shutdowncheck/internal/load"
	"github.com/shutdowncheck/shutdowncheck/internal/redact"
	runpkg "github.com/shutdowncheck/shutdowncheck/internal/run"
	"github.com/shutdowncheck/shutdowncheck/internal/target"
	"github.com/shutdowncheck/shutdowncheck/internal/timeline"
	"github.com/shutdowncheck/shutdowncheck/pkg/schema"
)

// Distinctive enough that finding one anywhere in an artefact is unambiguous.
const (
	querySecret    = "qs-DEADBEEF-must-never-appear"
	argvSecret     = "argv-DEADBEEF-must-never-appear"
	logSecret      = "log-DEADBEEF-must-never-appear"
	headerSecret   = "header-DEADBEEF-must-never-appear"
	passwordSecret = "pw-DEADBEEF-must-never-appear"
)

func allSecrets() []string {
	return []string{querySecret, argvSecret, logSecret, headerSecret, passwordSecret}
}

// leakyRecording is a run that was handed credentials by every route the tool
// records: the probed URL, the target's argv, an error string and a captured
// log line. If redaction is wired up correctly, none of them survive.
func leakyRecording(t *testing.T) string {
	t.Helper()

	rec := timeline.NewRecorder(timeline.Meta{
		ToolVersion: "test",
		// Labels are redacted where they are built, so this records what a real
		// run would actually have stored.
		Target:  redact.Argv([]string{"./api", "--db-password=" + passwordSecret}),
		Profile: "standalone",
	}, 0)

	leakyURL := "http://localhost:8080/orders?api_key=" + querySecret
	for range 12 {
		rec.Record(timeline.RequestAt(timeline.RequestEvent{
			Method: "GET", URL: redact.URL(leakyURL),
			Sent: 4900 * time.Millisecond, Done: 5100 * time.Millisecond,
			Status: 200, Outcome: timeline.OutcomeOK,
		}))
	}

	// net/http embeds the whole URL in its error strings, which is the
	// commonest accidental leak of all.
	rec.Record(timeline.RequestAt(timeline.RequestEvent{
		Method: "GET", URL: redact.URL(leakyURL),
		Sent: 5200 * time.Millisecond, Done: 5300 * time.Millisecond,
		Outcome: timeline.OutcomeRefused,
		Error:   redact.Message(`Get "` + leakyURL + `": connection refused`),
	}))

	rec.Record(timeline.LogAt(4*time.Second, "stdout",
		redact.Message(`starting with upstream "https://svc/?token=`+logSecret+`"`)))
	rec.Record(timeline.LogAt(4100*time.Millisecond, "stderr",
		redact.Message("request failed Authorization: Bearer "+headerSecret)))

	rec.Record(timeline.ReadinessAt(time.Second, timeline.ReadinessEvent{Status: 200, Healthy: true}))
	rec.Record(timeline.ReadinessAt(7*time.Second, timeline.ReadinessEvent{Status: 200, Healthy: true}))
	rec.Record(timeline.ListenerAt(4*time.Second, timeline.ListenerEvent{Accepting: true}))
	rec.Record(timeline.ListenerAt(5010*time.Millisecond, timeline.ListenerEvent{Accepting: false}))
	rec.Record(timeline.SignalAt(5*time.Second, timeline.SignalEvent{Signal: "TERM"}))

	code := 0
	rec.Record(timeline.ProcessAt(6*time.Second, timeline.ProcessEvent{Phase: timeline.ProcExited, ExitCode: &code}))

	path := filepath.Join(t.TempDir(), "leaky.ndjson")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { _ = f.Close() }()

	if err := timeline.WriteNDJSON(f, rec.Snapshot()); err != nil {
		t.Fatalf("WriteNDJSON: %v", err)
	}
	return path
}

// Spec section 14 requires that credentials never reach a report, a log line or
// the NDJSON stream. A report is uploaded to CI and shared, so a leak there is
// wider and longer-lived than the process table the value came from.
//
// This asserts on the bytes that actually get written, rather than on the
// redaction helpers in isolation: a helper that is correct but never called is
// the failure mode that matters.
func TestSecretsNeverReachAnyReportFormat(t *testing.T) {
	path := leakyRecording(t)

	for _, format := range []string{"json", "ndjson", "markdown", "junit", "human"} {
		t.Run(format, func(t *testing.T) {
			code, stdout, stderr := execute(t, "analyze", path, "--format", format, "--no-color")
			if code == schema.ExitUsage || code == schema.ExitInternal {
				t.Fatalf("exit = %d\nstderr: %s", code, stderr)
			}
			if stdout == "" {
				t.Fatal("no output, so this proved nothing")
			}
			assertNoSecrets(t, stdout)
		})
	}
}

func TestLiveAnalysisInputRedactsURLsAndKeepsCalibration(t *testing.T) {
	const secret = "live-report-secret-must-not-appear"
	baseline := 125 * time.Millisecond
	resolved := &config.Resolved{
		Target: config.Target{Kind: analyze.TargetDocker, Label: "docker(api)"},
		Probe: config.Probe{
			Requests: []config.Request{{
				URL:    "https://user:" + secret + "@example.test/work?token=" + secret,
				Method: "GET",
			}},
			ReadinessURL: "https://example.test/ready?api_key=" + secret,
		},
		Traffic: config.Traffic{EnsureInFlight: 20},
	}
	outcome := runpkg.Result{
		Calibrated: true,
		Calibration: load.Calibration{
			RPS: 160, BaselineLatency: baseline, Achievable: true,
		},
	}

	input := analysisInput(resolved, outcome, target.Descriptor{PID: 42}, "go-net-http")
	assertNoSecrets(t, input.Probe.URL+input.Probe.ReadinessURL)
	if input.Load.BaselineLatency != baseline {
		t.Errorf("baseline latency = %s, want %s", input.Load.BaselineLatency, baseline)
	}
}

// The written file is the artefact that leaves the machine.
func TestWrittenReportFileCarriesNoSecrets(t *testing.T) {
	recording := leakyRecording(t)
	out := filepath.Join(t.TempDir(), "report.json")

	code, stdout, stderr := execute(t, "analyze", recording, "--format", "json", "--no-color")
	if code == schema.ExitInternal {
		t.Fatalf("analyze failed: %s", stderr)
	}
	if err := os.WriteFile(out, []byte(stdout), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	data, err := os.ReadFile(out) // #nosec G304 -- test-controlled temp path
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	assertNoSecrets(t, string(data))

	var report schema.Report
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("the redacted report is no longer valid JSON: %v", err)
	}
	if report.Verdict == "" {
		t.Error("redaction destroyed the report's content")
	}
}

// A URL carrying a token in its query string is the commonest accidental leak.
func TestQueryStringSecretsAreRedacted(t *testing.T) {
	got := redact.URL("https://api.internal/orders?api_key=" + querySecret)

	assertNoSecrets(t, got)
	if !strings.Contains(got, redact.Placeholder) {
		t.Errorf("the value should be replaced rather than dropped silently: %s", got)
	}
	// The rest has to survive, or the report stops being diagnostic.
	if !strings.Contains(got, "api.internal") || !strings.Contains(got, "/orders") {
		t.Errorf("redaction destroyed the useful part of the URL: %s", got)
	}
}

// An argv is echoed back so a reader knows what was run, and passing a password
// on a command line is an ordinary way to start a service.
func TestCommandLineSecretsAreRedacted(t *testing.T) {
	cases := map[string][]string{
		"joined flag":     {"./api", "--db-password=" + passwordSecret},
		"separated flag":  {"./api", "--token", argvSecret},
		"env assignment":  {"API_TOKEN=" + argvSecret, "./api"},
		"underscore flag": {"./api", "--api_key=" + argvSecret},
		"single dash":     {"./api", "-secret", argvSecret},
		"colon flag":      {"./api", "--token:" + argvSecret},
		"windows flag":    {"./api.exe", "/password:" + argvSecret},
		"url in argv":     {"./api", "--upstream=https://x/y?secret=" + querySecret},
		"database URL":    {"./api", "--database-url=postgres://user:" + passwordSecret + "@db.internal/app"},
	}

	for name, argv := range cases {
		t.Run(name, func(t *testing.T) {
			got := redact.Argv(argv)
			assertNoSecrets(t, got)
			if !strings.Contains(got, "./api") {
				t.Errorf("the command itself should still be readable: %s", got)
			}
		})
	}
}

// Non-secret arguments must survive, or reports become unreadable and people
// stop believing the redaction is doing anything sensible.
func TestCommandLineRedactionLeavesOrdinaryArgumentsAlone(t *testing.T) {
	got := redact.Argv([]string{"./api", "--port", "8080", "--log-level=debug", "-v"})

	for _, want := range []string{"./api", "--port", "8080", "--log-level=debug", "-v"} {
		if !strings.Contains(got, want) {
			t.Errorf("redaction removed %q, which is not a secret: %s", want, got)
		}
	}
	if strings.Contains(got, redact.Placeholder) {
		t.Errorf("nothing here is sensitive, so nothing should be redacted: %s", got)
	}
}

func TestMalformedHeaderFlagDoesNotLeakItsValue(t *testing.T) {
	const secret = "header-parse-secret-must-not-appear"
	code, _, stderr := execute(t, "run", "--header", "Authorization "+secret)

	if code != schema.ExitUsage {
		t.Fatalf("exit = %d, want usage", code)
	}
	if strings.Contains(stderr, secret) {
		t.Fatalf("flag error leaked header value: %s", stderr)
	}
}

func TestRunHelpDoesNotLeakPreviouslyParsedHeaders(t *testing.T) {
	const secret = "help-header-secret-must-not-appear"
	code, _, stderr := execute(t, "run", "--header", "Authorization: "+secret, "--help")

	if code != schema.ExitPass {
		t.Fatalf("exit = %d, want success\nstderr: %s", code, stderr)
	}
	if strings.Contains(stderr, secret) {
		t.Fatalf("help output leaked a header value: %s", stderr)
	}
}

func TestSensitiveHeadersAreRecognised(t *testing.T) {
	for _, name := range []string{
		"Authorization", "authorization", "  Authorization  ",
		"Proxy-Authorization", "Cookie", "Set-Cookie", "X-Api-Key",
	} {
		if !redact.IsSensitiveHeader(name) {
			t.Errorf("%q should be treated as sensitive", name)
		}
	}
	for _, name := range []string{"Accept", "Content-Type", "User-Agent"} {
		if redact.IsSensitiveHeader(name) {
			t.Errorf("%q is not sensitive; redacting it would lose useful detail", name)
		}
	}
}

func assertNoSecrets(t *testing.T, content string) {
	t.Helper()

	for _, secret := range allSecrets() {
		if strings.Contains(content, secret) {
			t.Errorf("a secret reached the output: %q\nin:\n%s", secret, content)
		}
	}
}
