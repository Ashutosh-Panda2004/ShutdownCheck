package cli

import (
	"strings"
	"testing"

	"github.com/shutdowncheck/shutdowncheck/internal/target"
	"github.com/shutdowncheck/shutdowncheck/pkg/schema"
)

// The demo self-spawns the real binary, so the run itself is exercised in
// test/e2e where there is one to spawn. os.Executable inside a test points at
// the test binary, which would prove nothing about what ships.

func TestDemoServerIsNotAPublicCommand(t *testing.T) {
	code, _, stderr := execute(t, "__demo-server")

	if code != schema.ExitUsage {
		t.Fatalf("exit = %d, want usage", code)
	}
	if !strings.Contains(stderr, "not a supported command") {
		t.Errorf("the refusal should say it is internal: %s", stderr)
	}
}

func TestDemoServerIsAbsentFromHelp(t *testing.T) {
	_, stdout, _ := execute(t, "help")

	if strings.Contains(stdout, demoServerCommand) {
		t.Error("the hidden subcommand is listed in help, which invites people to depend on it")
	}
	if !strings.Contains(stdout, "demo") {
		t.Error("help should mention demo; it is the first thing a new user should run")
	}
}

func TestDemoRejectsUnknownFlags(t *testing.T) {
	if code, _, _ := execute(t, "demo", "--nonsense"); code != schema.ExitUsage {
		t.Errorf("exit = %d, want usage", code)
	}
}

// Where signals do not exist the demo must refuse rather than simulate, and the
// refusal has to carry the command that does work.
func TestDemoRefusesWithoutSignals(t *testing.T) {
	if target.ProcessTargetsSupported() {
		t.Skip("this platform has signals, so the demo runs for real; see test/e2e")
	}

	code, _, stderr := execute(t, "demo")
	if code != schema.ExitTarget {
		t.Fatalf("exit = %d, want %d", code, schema.ExitTarget)
	}
	for _, want := range []string{"--docker", "shutdowncheck run"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the refusal is missing %q, leaving the user with a dead end:\n%s", want, stderr)
		}
	}
	if strings.Contains(stderr, "VERDICT") {
		t.Error("a refusal must not look like a report")
	}
}
