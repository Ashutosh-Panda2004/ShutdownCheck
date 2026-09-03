//go:build !windows

// Package e2e drives the real binary against real processes.
//
// These are the only tests that exercise signal delivery, process-group
// cleanup and connection forensics against a genuinely independent server, so
// they run wherever POSIX signals exist. Windows is covered separately, where
// process targets fail loudly rather than pretending.
package e2e

import (
	"bytes"
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

var fixtureBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "shutdowncheck-e2e")
	if err != nil {
		fmt.Fprintln(os.Stderr, "temp dir:", err)
		os.Exit(1)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	fixtureBinary = filepath.Join(dir, "fixture")
	build := exec.Command("go", "build", "-o", fixtureBinary, "./testdata/fixture")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build fixture:", err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

// freePort reserves a port and releases it, so the fixture can be told exactly
// where to listen and the test knows the URL up front.
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

func runTool(t *testing.T, args ...string) outcome {
	t.Helper()

	var out, errOut bytes.Buffer
	code := cli.Main(args, &out, &errOut)
	return outcome{code: code, stdout: out.String(), stderr: errOut.String()}
}

func toolArgs(port int, mode string, extra ...string) []string {
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	args := []string{
		"run",
		"--url", "http://" + addr + "/work",
		"--readiness-url", "http://" + addr + "/readyz",
		"--ensure-in-flight", "6",
		"--warmup", "600ms",
		"--steady", "600ms",
		"--grace-period", "2s",
		"--no-color",
	}
	args = append(args, extra...)
	return append(args, "--", fixtureBinary, "-addr", addr, "-mode", mode)
}

// The headline case: a service that drains properly should pass cleanly, and
// the tool must be able to say so. If this ever fails, nothing else the tool
// reports can be trusted either.
func TestCorrectServicePasses(t *testing.T) {
	port := freePort(t)

	got := runTool(t, toolArgs(port, "correct", "--profile", "standalone")...)
	if got.code != schema.ExitPass {
		t.Fatalf("exit = %d, want pass\nstdout:\n%s\nstderr:\n%s", got.code, got.stdout, got.stderr)
	}
	if !strings.Contains(got.stdout, "VERDICT: PASS") {
		t.Errorf("stdout did not report a pass:\n%s", got.stdout)
	}
}

// A service that closes its listener the instant SIGTERM arrives is the
// widespread, subtle defect: correct by the usual advice, and a source of 502s
// behind any load balancer.
func TestInstantCloseIsCaughtUnderKubernetesProfile(t *testing.T) {
	port := freePort(t)

	got := runTool(t, toolArgs(port, "instant-close", "--profile", "kubernetes")...)
	if got.code != schema.ExitFail {
		t.Fatalf("exit = %d, want fail\nstdout:\n%s", got.code, got.stdout)
	}
	for _, want := range []string{"SC006", "SC007"} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("report does not mention %s:\n%s", want, got.stdout)
		}
	}
}

// The same evidence must reach the opposite conclusion under a profile where
// closing immediately is the requirement rather than the defect.
func TestInstantCloseIsAcceptedUnderStrictProfile(t *testing.T) {
	port := freePort(t)

	got := runTool(t, toolArgs(port, "instant-close", "--profile", "strict")...)
	if got.code == schema.ExitFail {
		t.Fatalf("exit = %d; closing immediately is correct under strict\nstdout:\n%s", got.code, got.stdout)
	}
}

func TestDroppedInFlightRequestsAreReported(t *testing.T) {
	port := freePort(t)

	got := runTool(t, toolArgs(port, "drop-inflight", "--profile", "standalone")...)
	if got.code != schema.ExitFail {
		t.Fatalf("exit = %d, want fail\nstdout:\n%s", got.code, got.stdout)
	}
	if !strings.Contains(got.stdout, "SC003") {
		t.Errorf("report does not mention dropped in-flight requests:\n%s", got.stdout)
	}
}

// A process that ignores the signal must be escalated past, not waited on
// forever, and the report has to say a hard kill was required.
func TestIgnoredSignalEscalatesToSigkill(t *testing.T) {
	port := freePort(t)

	got := runTool(t, toolArgs(port, "ignore-signal", "--profile", "standalone")...)
	if got.code != schema.ExitFail {
		t.Fatalf("exit = %d, want fail\nstdout:\n%s", got.code, got.stdout)
	}
	if !strings.Contains(got.stdout, "SC002") {
		t.Errorf("report does not mention that a kill was required:\n%s", got.stdout)
	}
}

func TestJSONOutputIsMachineReadable(t *testing.T) {
	port := freePort(t)

	got := runTool(t, toolArgs(port, "correct", "--profile", "standalone", "--format", "json")...)
	if got.code != schema.ExitPass {
		t.Fatalf("exit = %d\nstderr:\n%s", got.code, got.stderr)
	}

	for _, want := range []string{`"schema_version"`, `"verdict"`, `"by_phase"`} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("JSON output is missing %s", want)
		}
	}
}

func TestReportFilesAreWritten(t *testing.T) {
	port := freePort(t)
	dir := t.TempDir()

	reportPath := filepath.Join(dir, "report.json")
	badgePath := filepath.Join(dir, "badge.svg")

	got := runTool(t, toolArgs(port, "correct", "--profile", "standalone",
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
		// Reports carry internal hostnames and URLs.
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s has permissions %o, want 600", path, perm)
		}
	}
}

// Pointing the tool at nothing must be a target error, distinct from a defect
// in a service that does exist.
func TestUnreachableTargetIsATargetError(t *testing.T) {
	port := freePort(t)

	got := runTool(t,
		"run",
		"--url", fmt.Sprintf("http://127.0.0.1:%d/work", port),
		"--warmup", "200ms",
		"--steady", "200ms",
		"--grace-period", "1s",
		"--no-color",
		"--", fixtureBinary, "-addr", "127.0.0.1:0", "-mode", "correct",
	)

	if got.code != schema.ExitTarget && got.code != schema.ExitInconclusive {
		t.Fatalf("exit = %d, want a target or inconclusive result\nstderr:\n%s", got.code, got.stderr)
	}
}
