//go:build !windows

package target

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shutdowncheck/shutdowncheck/internal/clock"
)

// These exercise real signal delivery and process-group ownership, so they only
// run where POSIX signals exist. Windows behaviour is covered separately by
// TestWindowsRefusesProcessTargetsHonestly.

func newSleepTarget(t *testing.T, argv []string, sink LogSink) Target {
	t.Helper()

	tgt, err := NewCommand(CommandOptions{Argv: argv, LogSink: sink, GracePeriod: 5 * time.Second})
	if err != nil {
		t.Fatalf("NewCommand: %v", err)
	}
	t.Cleanup(func() { _ = tgt.Close() })
	return tgt
}

func TestCommandTargetTerminatesOnSignal(t *testing.T) {
	tgt := newSleepTarget(t, []string{"sleep", "60"}, nil)

	if err := tgt.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if pid := tgt.Describe().PID; pid <= 0 {
		t.Fatalf("PID = %d, want a real process id", pid)
	}

	alive, err := tgt.Alive()
	if err != nil || !alive {
		t.Fatalf("Alive() = %v, %v; want true", alive, err)
	}

	if err := tgt.Signal(SIGTERM); err != nil {
		t.Fatalf("Signal: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status, err := tgt.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !status.Known {
		t.Fatal("a spawned child's exit status must be observable")
	}
	if !status.Signaled || status.TerminatedBy != SIGTERM {
		t.Errorf("status = %s, want killed by SIGTERM", status)
	}
}

func TestCommandTargetReportsExitCode(t *testing.T) {
	tgt := newSleepTarget(t, []string{"sh", "-c", "exit 7"}, nil)

	if err := tgt.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status, err := tgt.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !status.Known || status.Code != 7 {
		t.Fatalf("status = %s, want exit 7", status)
	}
}

// A service that traps SIGTERM is the SC001/SC002 case, and the tool has to be
// able to escalate past it rather than hang.
func TestCommandTargetCanBeKilledWhenItIgnoresSigterm(t *testing.T) {
	tgt := newSleepTarget(t, []string{"sh", "-c", `trap "" TERM; sleep 60`}, nil)

	if err := tgt.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := tgt.Signal(SIGTERM); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	if alive, _ := tgt.Alive(); !alive {
		t.Fatal("the target trapped SIGTERM and should still be running")
	}

	if err := tgt.Signal(SIGKILL); err != nil {
		t.Fatalf("SIGKILL: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := tgt.Wait(ctx); err != nil {
		t.Fatalf("Wait after kill: %v", err)
	}
}

// Signals must reach the whole group. A service whose workers survive the
// parent is exactly the orphaned-listener defect SC012 describes, and cleanup
// that leaked them would poison every later run on the same machine.
func TestCommandTargetSignalsTheWholeProcessGroup(t *testing.T) {
	// The parent spawns a child and exits the shell's foreground, leaving the
	// child in the same process group.
	tgt := newSleepTarget(t, []string{"sh", "-c", `sleep 60 & echo $!; wait`}, nil)

	if err := tgt.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(150 * time.Millisecond)

	pgid := tgt.Describe().PID
	if err := tgt.Signal(SIGKILL); err != nil {
		t.Fatalf("Signal: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = tgt.Wait(ctx)

	// Nothing in the group should answer a probe signal.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("members of the process group survived SIGKILL")
}

func TestCommandTargetCloseKillsSurvivors(t *testing.T) {
	tgt, err := NewCommand(CommandOptions{Argv: []string{"sleep", "60"}})
	if err != nil {
		t.Fatalf("NewCommand: %v", err)
	}
	if err := tgt.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pgid := tgt.Describe().PID

	if err := tgt.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := tgt.Close(); err != nil {
		t.Fatalf("Close must be idempotent, second call returned: %v", err)
	}

	if err := syscall.Kill(-pgid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("process group survived Close (kill probe returned %v)", err)
	}
}

func TestCommandTargetCapturesLogs(t *testing.T) {
	c := &captured{}
	tgt := newSleepTarget(t, []string{"sh", "-c", `echo to-stdout; echo to-stderr 1>&2`}, c.sink)

	if err := tgt.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := tgt.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	var sawOut, sawErr bool
	for _, line := range c.all() {
		sawOut = sawOut || strings.Contains(line, "stdout: to-stdout")
		sawErr = sawErr || strings.Contains(line, "stderr: to-stderr")
	}
	if !sawOut || !sawErr {
		t.Fatalf("captured %v, want both streams", c.all())
	}
}

func TestCommandTargetStartFailsForMissingBinary(t *testing.T) {
	tgt, err := NewCommand(CommandOptions{Argv: []string{"shutdowncheck-no-such-binary"}})
	if err != nil {
		t.Fatalf("NewCommand: %v", err)
	}
	defer func() { _ = tgt.Close() }()

	if err := tgt.Start(context.Background()); err == nil {
		t.Fatal("starting a missing binary should fail")
	}
}

func TestCommandTargetSignalBeforeStart(t *testing.T) {
	tgt, err := NewCommand(CommandOptions{Argv: []string{"sleep", "60"}})
	if err != nil {
		t.Fatalf("NewCommand: %v", err)
	}
	defer func() { _ = tgt.Close() }()

	if err := tgt.Signal(SIGTERM); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("Signal before Start = %v, want ErrNotStarted", err)
	}
}

func TestCommandTargetRejectsSecondStart(t *testing.T) {
	tgt := newSleepTarget(t, []string{"sleep", "60"}, nil)

	if err := tgt.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := tgt.Start(context.Background()); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("second Start = %v, want ErrAlreadyStarted", err)
	}
}

func TestCommandTargetStartFailsWhenReadinessNeverComes(t *testing.T) {
	tgt, err := NewCommand(CommandOptions{
		Argv:  []string{"sleep", "60"},
		Ready: ReadyCheck{Addr: "127.0.0.1:1", Timeout: 150 * time.Millisecond, Interval: 10 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("NewCommand: %v", err)
	}
	defer func() { _ = tgt.Close() }()

	startErr := tgt.Start(context.Background())
	if !IsReadyTimeout(startErr) {
		t.Fatalf("Start = %v, want a readiness timeout", startErr)
	}

	// Start must not leave the process running after giving up on it.
	if alive, _ := tgt.Alive(); alive {
		t.Error("the target was left running after readiness failed")
	}
}

// An attached process is not our child, so its exit code genuinely cannot be
// observed. It has to be reported as unknown rather than invented.
func TestAttachedProcessReportsUnknownExitStatus(t *testing.T) {
	helper := newSleepTarget(t, []string{"sleep", "60"}, nil)
	if err := helper.Start(context.Background()); err != nil {
		t.Fatalf("Start helper: %v", err)
	}
	pid := helper.Describe().PID

	tgt, err := NewProcess(ProcessOptions{PID: pid})
	if err != nil {
		t.Fatalf("NewProcess: %v", err)
	}
	defer func() { _ = tgt.Close() }()

	if alive, err := tgt.Alive(); err != nil || !alive {
		t.Fatalf("Alive() = %v, %v; want true", alive, err)
	}

	if err := tgt.Signal(SIGKILL); err != nil {
		t.Fatalf("Signal: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status, err := tgt.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if status.Known {
		t.Fatal("an attached process's exit code is unavailable and must be reported as unknown")
	}
}

// Cleanup must never destroy a process the tool did not start.
func TestAttachedProcessCloseDoesNotKill(t *testing.T) {
	helper := newSleepTarget(t, []string{"sleep", "60"}, nil)
	if err := helper.Start(context.Background()); err != nil {
		t.Fatalf("Start helper: %v", err)
	}

	tgt, err := NewProcess(ProcessOptions{PID: helper.Describe().PID})
	if err != nil {
		t.Fatalf("NewProcess: %v", err)
	}
	if err := tgt.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if alive, _ := helper.Alive(); !alive {
		t.Fatal("closing an attached target killed a process the tool did not own")
	}
}

func TestNewProcessRejectsMissingPID(t *testing.T) {
	// Find a pid that is almost certainly free.
	if _, err := NewProcess(ProcessOptions{PID: 0x7FFFFFF0}); err == nil {
		t.Fatal("attaching to a non-existent PID should fail")
	}
}

func TestWaitReadyAgainstSpawnedServer(t *testing.T) {
	if _, err := exec.LookPath("nc"); err != nil {
		t.Skip("nc is not available")
	}

	tgt, err := NewCommand(CommandOptions{
		Argv:  []string{"sh", "-c", "sleep 0.2; exec nc -l 127.0.0.1 18099"},
		Ready: ReadyCheck{Addr: "127.0.0.1:18099", Timeout: 3 * time.Second, Interval: 20 * time.Millisecond},
		Clock: clock.System(),
	})
	if err != nil {
		t.Fatalf("NewCommand: %v", err)
	}
	defer func() { _ = tgt.Close() }()

	if err := tgt.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
}
