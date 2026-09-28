//go:build !windows

package e2e

import (
	"bytes"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/Ashutosh-Panda2004/ShutdownCheck/pkg/schema"
)

func runBinary(t *testing.T, args ...string) outcome {
	t.Helper()

	var out, errOut bytes.Buffer
	cmd := exec.Command(toolBinary, args...)
	cmd.Stdout = &out
	cmd.Stderr = &errOut

	code := 0
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("running %v: %v", args, err)
		}
		code = exit.ExitCode()
	}
	return outcome{code: code, stdout: out.String(), stderr: errOut.String()}
}

// The demo is the first thing a new user runs, so it has to work and it has to
// find the defect it advertises. It is exercised through a real build because
// it re-executes os.Executable, which inside a test would be the test binary.
func TestDemoFindsTheDefectItAdvertises(t *testing.T) {
	got := runBinary(t, "demo", "--no-color")

	if got.code != schema.ExitFail {
		t.Fatalf("exit = %d, want fail; the demo service is deliberately broken\nstdout:\n%s\nstderr:\n%s",
			got.code, got.stdout, got.stderr)
	}

	// Closing the listener on SIGTERM with no readiness flip is the mistake the
	// preamble promises to demonstrate.
	for _, want := range []string{"VERDICT: FAIL", "SC006", "SC007"} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("the demo report is missing %q:\n%s", want, got.stdout)
		}
	}
	if !strings.Contains(got.stderr, "deliberately broken") {
		t.Errorf("the demo should say up front that the service is broken on purpose:\n%s", got.stderr)
	}
}

// The same evidence has to reach a different conclusion where closing the
// listener immediately is the requirement rather than the defect.
func TestDemoProfileChangesTheVerdict(t *testing.T) {
	strict := runBinary(t, "demo", "--profile", "strict", "--no-color")

	if strict.code == schema.ExitInternal {
		t.Fatalf("demo failed to run: %s", strict.stderr)
	}
	if strings.Contains(strict.stdout, "SC006") {
		t.Errorf("SC006 should not fire under strict, where closing immediately is correct:\n%s", strict.stdout)
	}
}

func TestDemoMachineReadableOutput(t *testing.T) {
	got := runBinary(t, "demo", "--format", "json")

	if got.code != schema.ExitFail {
		t.Fatalf("exit = %d\nstderr:\n%s", got.code, got.stderr)
	}
	if !strings.HasPrefix(strings.TrimSpace(got.stdout), "{") {
		t.Errorf("--format json did not produce JSON on stdout:\n%s", got.stdout)
	}
	// The preamble is commentary and must not contaminate machine-readable
	// output on stdout.
	if strings.Contains(got.stdout, "deliberately broken") {
		t.Error("the preamble leaked into stdout, corrupting the JSON stream")
	}
}
