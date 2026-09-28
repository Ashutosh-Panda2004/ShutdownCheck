package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/timeline"
	"github.com/Ashutosh-Panda2004/ShutdownCheck/pkg/schema"
)

func execute(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()

	var out, errOut bytes.Buffer
	code = Main(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// The exit codes are the contract a CI pipeline depends on: they have to
// distinguish "your service has a bug" from "you invoked me wrong".
func TestExitCodes(t *testing.T) {
	cases := map[string]struct {
		args []string
		want int
	}{
		"no arguments":       {nil, schema.ExitUsage},
		"unknown command":    {[]string{"nonsense"}, schema.ExitUsage},
		"version":            {[]string{"version"}, schema.ExitPass},
		"help":               {[]string{"help"}, schema.ExitPass},
		"explain list":       {[]string{"explain"}, schema.ExitPass},
		"explain a rule":     {[]string{"explain", "SC006"}, schema.ExitPass},
		"explain unknown":    {[]string{"explain", "SC999"}, schema.ExitUsage},
		"run with no target": {[]string{"run", "--url", "http://localhost:8080/"}, schema.ExitUsage},
		"run with no url":    {[]string{"run", "--pid", "4242"}, schema.ExitUsage},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got, _, _ := execute(t, tc.args...); got != tc.want {
				t.Errorf("exit code = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestSubcommandHelpExitsSuccessfully(t *testing.T) {
	for _, command := range []string{"run", "analyze", "demo"} {
		t.Run(command, func(t *testing.T) {
			code, _, stderr := execute(t, command, "--help")
			if code != schema.ExitPass {
				t.Fatalf("exit = %d, want 0\nstderr: %s", code, stderr)
			}
			if stderr == "" {
				t.Fatal("help output is empty")
			}
		})
	}
}

func TestCanceledRunUsesInterruptedExitCode(t *testing.T) {
	var stderr bytes.Buffer
	if got := exitFor(&targetError{context.Canceled}, &stderr); got != schema.ExitInterrupted {
		t.Errorf("exit = %d, want %d", got, schema.ExitInterrupted)
	}
	if !strings.Contains(stderr.String(), "interrupted") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestVersionOutput(t *testing.T) {
	code, stdout, _ := execute(t, "version")
	if code != schema.ExitPass {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout, "shutdowncheck") {
		t.Errorf("version output = %q", stdout)
	}
}

func TestUsageMentionsEveryExitCode(t *testing.T) {
	_, stdout, _ := execute(t, "help")

	for _, want := range []string{"pass", "fail", "inconclusive", "usage", "target", "internal"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("usage text does not document the %q exit code", want)
		}
	}
}

func TestExplainListsEverySignature(t *testing.T) {
	_, stdout, _ := execute(t, "explain")

	for _, id := range []string{"SC000", "SC003", "SC006", "SC017"} {
		if !strings.Contains(stdout, id) {
			t.Errorf("signature list omits %s", id)
		}
	}
}

func TestExplainIsCaseInsensitive(t *testing.T) {
	code, stdout, _ := execute(t, "explain", "sc006")
	if code != schema.ExitPass {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout, "NO_DEREGISTRATION_WINDOW") {
		t.Errorf("explain output = %q", stdout[:min(200, len(stdout))])
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "shutdowncheck.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestValidateAcceptsAGoodConfig(t *testing.T) {
	path := writeConfig(t, `
version: 1
defaults:
  profile: kubernetes
scenarios:
  api:
    target:
      command: ["./bin/api"]
    probe:
      readiness_url: http://localhost:8080/readyz
      requests:
        - url: http://localhost:8080/api/orders
`)

	code, stdout, stderr := execute(t, "validate", "--config", path)
	if code != schema.ExitPass {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "api") || !strings.Contains(stdout, "kubernetes") {
		t.Errorf("validate output = %q", stdout)
	}
}

func TestValidateRejectsABadConfig(t *testing.T) {
	path := writeConfig(t, `
version: 1
scenarios:
  api:
    target: {}
    probe:
      requests:
        - url: not-a-url
`)

	code, _, stderr := execute(t, "validate", "--config", path)
	if code != schema.ExitUsage {
		t.Fatalf("exit = %d, want %d", code, schema.ExitUsage)
	}
	if !strings.Contains(stderr, "target") {
		t.Errorf("stderr should name the problem, got %q", stderr)
	}
}

func TestValidateReportsAMissingFile(t *testing.T) {
	code, _, _ := execute(t, "validate", "--config", filepath.Join(t.TempDir(), "absent.yaml"))
	if code != schema.ExitUsage {
		t.Fatalf("exit = %d, want %d", code, schema.ExitUsage)
	}
}

func TestRunRejectsConflictingTargets(t *testing.T) {
	code, _, stderr := execute(t, "run", "--url", "http://localhost:8080/", "--pid", "4242", "--docker", "api")
	if code != schema.ExitUsage {
		t.Fatalf("exit = %d, want %d", code, schema.ExitUsage)
	}
	if !strings.Contains(stderr, "exactly one") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestRunRejectsUnknownFormat(t *testing.T) {
	code, _, stderr := execute(t, "run", "--url", "http://localhost:8080/", "--pid", "4242", "--format", "html")
	if code != schema.ExitUsage {
		t.Fatalf("exit = %d, want %d", code, schema.ExitUsage)
	}
	if !strings.Contains(stderr, "html") {
		t.Errorf("stderr should name the bad format, got %q", stderr)
	}
}

// A misspelled signature in --ignore would silently suppress nothing, so it has
// to be rejected rather than accepted and quietly discarded.
func TestRunRejectsUnknownSignatureOverrides(t *testing.T) {
	for _, flagName := range []string{"--ignore", "--fail-on"} {
		code, _, stderr := execute(t, "run", "--url", "http://localhost:8080/", "--pid", "4242", flagName, "SC999")
		if code != schema.ExitUsage {
			t.Errorf("%s: exit = %d, want %d", flagName, code, schema.ExitUsage)
		}
		if !strings.Contains(stderr, "SC999") {
			t.Errorf("%s: stderr should name the bad id, got %q", flagName, stderr)
		}
	}
}

func TestRunRejectsBothBodyAndBodyFile(t *testing.T) {
	code, _, _ := execute(t, "run", "--url", "http://x/", "--pid", "4242", "--body", "a", "--body-file", "b")
	if code != schema.ExitUsage {
		t.Fatalf("exit = %d, want %d", code, schema.ExitUsage)
	}
}

func TestRunRejectsUnsafePID(t *testing.T) {
	// PID 1 would take down the host or container.
	code, _, stderr := execute(t, "run", "--url", "http://localhost:8080/", "--pid", "1")
	if code == schema.ExitPass {
		t.Fatal("targeting PID 1 must never be accepted")
	}
	if !strings.Contains(stderr, "1") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestHeaderFlagParsing(t *testing.T) {
	var h headerList

	if err := h.Set("Content-Type: application/json"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := h.Set("X-Trace:abc"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if h["Content-Type"] != "application/json" || h["X-Trace"] != "abc" {
		t.Errorf("headers = %v", map[string]string(h))
	}
	if err := h.Set("x-trace:replacement"); err != nil {
		t.Fatalf("Set case variant: %v", err)
	}
	if len(h) != 2 || h["X-Trace"] != "replacement" {
		t.Errorf("case variants were not canonicalized: %v", map[string]string(h))
	}

	for _, bad := range []string{"no-colon", ": empty-name", "Bad Header: value", "X-Test: ok\r\ninjected: value"} {
		if err := h.Set(bad); err == nil {
			t.Errorf("Set(%q) should have failed", bad)
		}
	}
}

func TestIDListAcceptsCommasAndRepetition(t *testing.T) {
	var ids idList

	if err := ids.Set("sc003,SC006"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := ids.Set("SC009"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	want := []string{"SC003", "SC006", "SC009"}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("ids[%d] = %q, want %q", i, ids[i], want[i])
		}
	}
}

func TestRepeatedStringFlag(t *testing.T) {
	var r repeatedString

	if err := r.Set("a"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := r.Set(""); err == nil {
		t.Error("an empty value should be rejected")
	}
	if r.String() != "a" {
		t.Errorf("String() = %q", r.String())
	}
}

// instantCloseNDJSON records a service that closes its listener the moment the
// signal arrives, and never flips readiness.
func instantCloseNDJSON(t *testing.T) string {
	t.Helper()

	rec := timeline.NewRecorder(timeline.Meta{ToolVersion: "test", Target: "./api", Profile: "standalone"}, 0)
	for range 12 {
		rec.Record(timeline.RequestAt(timeline.RequestEvent{
			Method: "GET", URL: "http://localhost:8080/", Sent: 4900 * time.Millisecond,
			Done: 5100 * time.Millisecond, Status: 200, Outcome: timeline.OutcomeOK,
		}))
	}
	rec.Record(timeline.ReadinessAt(time.Second, timeline.ReadinessEvent{Status: 200, Healthy: true}))
	rec.Record(timeline.ReadinessAt(7*time.Second, timeline.ReadinessEvent{Status: 200, Healthy: true}))
	rec.Record(timeline.ListenerAt(4*time.Second, timeline.ListenerEvent{Accepting: true}))
	rec.Record(timeline.ListenerAt(5010*time.Millisecond, timeline.ListenerEvent{Accepting: false}))
	rec.Record(timeline.SignalAt(5*time.Second, timeline.SignalEvent{Signal: "TERM"}))

	code := 0
	rec.Record(timeline.ProcessAt(6*time.Second, timeline.ProcessEvent{Phase: timeline.ProcExited, ExitCode: &code}))

	path := filepath.Join(t.TempDir(), "run.ndjson")
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

// Re-judging recorded evidence under a different deployment model is what
// separating measurement from interpretation buys. The same run is correct
// standalone and a source of 502s behind a load balancer.
func TestAnalyzeReJudgesRecordedEvidence(t *testing.T) {
	path := instantCloseNDJSON(t)

	standalone, stdout, stderr := execute(t, "analyze", path, "--profile", "standalone", "--no-color")
	if standalone != schema.ExitPass {
		t.Fatalf("standalone exit = %d, want pass\nstdout:\n%s\nstderr:\n%s", standalone, stdout, stderr)
	}

	kubernetes, stdout, _ := execute(t, "analyze", path, "--profile", "kubernetes", "--no-color")
	if kubernetes != schema.ExitFail {
		t.Fatalf("kubernetes exit = %d, want fail\nstdout:\n%s", kubernetes, stdout)
	}
	if !strings.Contains(stdout, "SC006") {
		t.Errorf("the kubernetes verdict should name SC006:\n%s", stdout)
	}
}

func TestAnalyzeUsesTheRecordedProfileByDefault(t *testing.T) {
	path := instantCloseNDJSON(t)

	code, _, _ := execute(t, "analyze", path, "--no-color")
	if code != schema.ExitPass {
		t.Fatalf("exit = %d; the recording says standalone, under which this run passes", code)
	}
}

func TestAnalyzeSupportsEveryFormat(t *testing.T) {
	path := instantCloseNDJSON(t)

	for _, format := range []string{"json", "junit", "markdown", "ndjson"} {
		code, stdout, stderr := execute(t, "analyze", path, "--format", format, "--profile", "standalone")
		if code != schema.ExitPass {
			t.Errorf("%s: exit = %d, stderr = %s", format, code, stderr)
		}
		if stdout == "" {
			t.Errorf("%s: produced no output", format)
		}
	}
}

func TestAnalyzeRejectsBadInput(t *testing.T) {
	cases := map[string][]string{
		"no file":      {"analyze"},
		"missing file": {"analyze", filepath.Join(t.TempDir(), "absent.ndjson")},
		"bad format":   {"analyze", instantCloseNDJSON(t), "--format", "html"},
		"bad profile":  {"analyze", instantCloseNDJSON(t), "--profile", "nonsense"},
	}

	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if code, _, _ := execute(t, args...); code != schema.ExitUsage {
				t.Errorf("exit = %d, want %d", code, schema.ExitUsage)
			}
		})
	}
}

func TestAnalyzeRejectsMalformedEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.ndjson")
	if err := os.WriteFile(path, []byte("{not json}"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if code, _, _ := execute(t, "analyze", path); code != schema.ExitUsage {
		t.Fatalf("exit = %d, want %d", code, schema.ExitUsage)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
