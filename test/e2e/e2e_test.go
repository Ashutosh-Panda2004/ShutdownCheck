//go:build !windows

// Package e2e covers the command-line surface end to end: what gets written
// where, in what format, and with which exit code.
//
// Verdict correctness across stacks is the conformance suite's job. This is
// deliberately about the parts a user touches, so the two do not drift into
// testing the same thing twice. Both need POSIX signals, so both are Unix-only.
package e2e

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shutdowncheck/shutdowncheck/internal/cli"
	"github.com/shutdowncheck/shutdowncheck/pkg/schema"
)

var serverBinary string

// toolBinary is the real shutdowncheck executable. The demo re-executes
// os.Executable, so it can only be exercised through a genuine build; inside a
// test that would point at the test binary and prove nothing about what ships.
var toolBinary string

func TestMain(m *testing.M) {
	// testing.Short is only readable once flags are parsed, and TestMain runs
	// before the testing package does that itself.
	flag.Parse()
	if testing.Short() {
		// Nothing here runs without building and killing real processes.
		os.Exit(0)
	}

	dir, err := os.MkdirTemp("", "shutdowncheck-e2e")
	if err != nil {
		fmt.Fprintln(os.Stderr, "temp dir:", err)
		os.Exit(1)
	}

	// The conformance server is the single fixture for the whole project; a
	// second copy here would be one more thing to keep in step.
	serverBinary = filepath.Join(dir, "server")
	toolBinary = filepath.Join(dir, "shutdowncheck")

	for _, build := range []struct{ out, pkg string }{
		{serverBinary, "../conformance/go"},
		{toolBinary, "../../cmd/shutdowncheck"},
	} {
		cmd := exec.Command("go", "build", "-o", build.out, build.pkg)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "build %s: %v\n", build.pkg, err)
			_ = os.RemoveAll(dir)
			os.Exit(1)
		}
	}

	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func freePort(t *testing.T) int {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

type outcome struct {
	code   int
	stdout string
	stderr string
}

func runTool(args ...string) outcome {
	var out, errOut bytes.Buffer
	code := cli.Main(args, &out, &errOut)
	return outcome{code: code, stdout: out.String(), stderr: errOut.String()}
}

func toolArgs(t *testing.T, mode string, extra ...string) []string {
	t.Helper()

	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))

	args := []string{
		"run",
		"--url", "http://" + addr + "/work",
		"--readiness-url", "http://" + addr + "/readyz",
		"--profile", "standalone",
		"--ensure-in-flight", "6",
		"--warmup", "600ms",
		"--steady", "700ms",
		"--grace-period", "5s",
		"--no-color",
	}
	args = append(args, extra...)
	return append(args, "--", serverBinary, "-addr", addr, "-mode", mode)
}

// The sanity anchor for everything else: if a correct service cannot pass,
// nothing else the tool reports can be trusted.
func TestCorrectServicePassesEndToEnd(t *testing.T) {
	got := runTool(toolArgs(t, "correct")...)
	if got.code != schema.ExitPass {
		t.Fatalf("exit = %d, want pass\nstdout:\n%s\nstderr:\n%s", got.code, got.stdout, got.stderr)
	}
	for _, want := range []string{"VERDICT: PASS", "in flight at signal"} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("report is missing %q:\n%s", want, got.stdout)
		}
	}
}

func TestTimelineIsShownUnlessSuppressed(t *testing.T) {
	shown := runTool(toolArgs(t, "correct")...)
	if !strings.Contains(shown.stdout, "TIMELINE") {
		t.Errorf("the timeline should be shown by default:\n%s", shown.stdout)
	}

	hidden := runTool(toolArgs(t, "correct", "--no-timeline")...)
	if strings.Contains(hidden.stdout, "TIMELINE") {
		t.Errorf("--no-timeline should suppress it:\n%s", hidden.stdout)
	}
}

func TestJSONOutputParsesAsTheVersionedSchema(t *testing.T) {
	got := runTool(toolArgs(t, "correct", "--format", "json")...)
	if got.code != schema.ExitPass {
		t.Fatalf("exit = %d\nstderr:\n%s", got.code, got.stderr)
	}

	var report schema.Report
	if err := json.Unmarshal([]byte(got.stdout), &report); err != nil {
		t.Fatalf("output is not valid schema JSON: %v\n%s", err, got.stdout)
	}
	if report.SchemaVersion != schema.SchemaVersion {
		t.Errorf("schema_version = %q, want %q", report.SchemaVersion, schema.SchemaVersion)
	}
	if report.Verdict != schema.VerdictPass {
		t.Errorf("verdict = %q", report.Verdict)
	}
	if report.Requests.ByPhase.InFlight.Count == 0 {
		t.Error("no requests were in flight at the signal, so the run proved nothing")
	}
}

func TestReportFilesAreWrittenWithRestrictivePermissions(t *testing.T) {
	dir := t.TempDir()
	reportPath := filepath.Join(dir, "report.json")
	badgePath := filepath.Join(dir, "badge.svg")

	got := runTool(toolArgs(t, "correct",
		"--format", "json", "--output", reportPath, "--badge", badgePath)...)
	if got.code != schema.ExitPass {
		t.Fatalf("exit = %d\nstderr:\n%s", got.code, got.stderr)
	}

	for _, path := range []string{reportPath, badgePath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("stat %s: %v", path, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("%s is empty", path)
		}
		// Reports carry internal hostnames and URLs, so they are not
		// world-readable.
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s has permissions %o, want 600", path, perm)
		}
	}
}

// Recording a run and re-judging it later is what separating measurement from
// interpretation buys, so the round trip is exercised against a real run rather
// than a synthetic timeline.
func TestRecordedRunCanBeReJudgedUnderAnotherProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.ndjson")

	got := runTool(toolArgs(t, "instant-close", "--format", "ndjson", "--output", path)...)
	if got.code == schema.ExitInternal || got.code == schema.ExitTarget {
		t.Fatalf("run failed: %s", got.stderr)
	}

	if info, err := os.Stat(path); err != nil || info.Size() == 0 {
		t.Fatalf("no evidence was recorded: %v", err)
	}

	// The same evidence is acceptable standalone and a source of 502s behind
	// a load balancer.
	reJudged := runTool("analyze", path, "--profile", "kubernetes", "--no-color")
	if reJudged.code != schema.ExitFail {
		t.Fatalf("re-analysis exit = %d, want fail\n%s", reJudged.code, reJudged.stdout)
	}
	if !strings.Contains(reJudged.stdout, "SC006") {
		t.Errorf("re-analysis should report SC006:\n%s", reJudged.stdout)
	}
}

func TestMultipleTrialsAreAggregated(t *testing.T) {
	got := runTool(toolArgs(t, "correct", "--trials", "2", "--format", "json")...)
	if got.code != schema.ExitPass {
		t.Fatalf("exit = %d\nstderr:\n%s", got.code, got.stderr)
	}

	var report schema.Report
	if err := json.Unmarshal([]byte(got.stdout), &report); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if report.Run.Trials.Total != 2 {
		t.Errorf("trials.total = %d, want 2", report.Run.Trials.Total)
	}
}

// Pointing the tool at nothing is a different failure from finding a defect,
// and a pipeline needs to be able to tell them apart.
func TestUnreachableTargetIsNotReportedAsADefect(t *testing.T) {
	port := freePort(t)

	got := runTool(
		"run",
		"--url", fmt.Sprintf("http://127.0.0.1:%d/work", port),
		"--warmup", "200ms",
		"--steady", "200ms",
		"--grace-period", "1s",
		"--no-color",
		"--", serverBinary, "-addr", "127.0.0.1:0", "-mode", "correct",
	)

	if got.code == schema.ExitPass || got.code == schema.ExitFail {
		t.Fatalf("exit = %d; an unreachable target is neither a pass nor a defect\nstderr:\n%s",
			got.code, got.stderr)
	}
}

func TestCapturedTargetLogsReachTheEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.ndjson")

	got := runTool(toolArgs(t, "correct",
		"--capture-target-logs", "--format", "ndjson", "--output", path)...)
	if got.code != schema.ExitPass {
		t.Fatalf("exit = %d\nstderr:\n%s", got.code, got.stderr)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), `"kind":"log"`) {
		t.Error("target output was not captured onto the timeline")
	}
}
