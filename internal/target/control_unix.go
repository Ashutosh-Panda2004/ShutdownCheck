//go:build !windows

package target

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

const attachPollInterval = 10 * time.Millisecond

// ProcessTargetsSupported reports whether this platform can deliver the signals
// a process or command target needs.
func ProcessTargetsSupported() bool { return true }

func osSignal(s Signal) (syscall.Signal, error) {
	switch s {
	case SIGTERM:
		return syscall.SIGTERM, nil
	case SIGINT:
		return syscall.SIGINT, nil
	case SIGQUIT:
		return syscall.SIGQUIT, nil
	case SIGKILL:
		return syscall.SIGKILL, nil
	default:
		return 0, fmt.Errorf("unsupported signal %q", s)
	}
}

func signalName(s syscall.Signal) Signal {
	switch s {
	case syscall.SIGTERM:
		return SIGTERM
	case syscall.SIGINT:
		return SIGINT
	case syscall.SIGQUIT:
		return SIGQUIT
	case syscall.SIGKILL:
		return SIGKILL
	default:
		return Signal(s.String())
	}
}

// commandControl spawns a process in its own group.
//
// The group matters twice over: an orchestrator signals a whole container, and
// cleanup must reach children the target spawned. Signalling only the parent
// would leave workers holding the port after the run, which is the very defect
// SC012 exists to catch.
type commandControl struct {
	cmd           *exec.Cmd
	pgid          int
	done          chan struct{}
	stdout        io.Writer
	stderr        io.Writer
	status        ExitStatus
	waitMu        sync.Mutex
	waited        bool
	groupSurvived bool
}

func newCommandControl(opts CommandOptions, stdout, stderr io.Writer) (processControl, error) {
	// Executed directly from an argv slice; no shell, no interpolation.
	cmd := exec.Command(opts.Argv[0], opts.Argv[1:]...) // #nosec G204 -- argv comes from the operator's own config
	cmd.Dir = opts.Dir
	cmd.Env = opts.Env
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	return &commandControl{cmd: cmd, done: make(chan struct{}), stdout: stdout, stderr: stderr}, nil
}

func (c *commandControl) Start() error {
	if err := c.cmd.Start(); err != nil {
		return fmt.Errorf("start target: %w", err)
	}
	c.pgid = c.cmd.Process.Pid

	go func() {
		err := c.cmd.Wait()
		flushLogWriter(c.stdout)
		flushLogWriter(c.stderr)
		groupErr := syscall.Kill(-c.pgid, 0)

		c.waitMu.Lock()
		c.status = exitStatusFrom(c.cmd.ProcessState, err)
		c.waited = true
		c.groupSurvived = groupErr == nil || errors.Is(groupErr, syscall.EPERM)
		c.waitMu.Unlock()

		close(c.done)
	}()
	return nil
}

func (c *commandControl) PID() int {
	if c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

func (c *commandControl) SignalPID(sig Signal) error {
	if c.cmd.Process == nil {
		return ErrNotStarted
	}
	osSig, err := osSignal(sig)
	if err != nil {
		return err
	}

	// Negative pid addresses the whole process group.
	if err := syscall.Kill(-c.pgid, osSig); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil // already gone
		}
		return fmt.Errorf("signal process group %d: %w", c.pgid, err)
	}
	return nil
}

func (c *commandControl) Alive() (bool, error) {
	select {
	case <-c.done:
		return false, nil
	default:
		return true, nil
	}
}

func (c *commandControl) Wait(ctx context.Context) (ExitStatus, error) {
	select {
	case <-c.done:
	case <-ctx.Done():
		return ExitStatus{}, ctx.Err()
	}

	c.waitMu.Lock()
	defer c.waitMu.Unlock()
	return c.status, nil
}

func (c *commandControl) Cleanup() error {
	if c.cmd.Process == nil {
		return nil
	}

	select {
	case <-c.done:
		c.waitMu.Lock()
		groupSurvived := c.groupSurvived
		c.waitMu.Unlock()
		if !groupSurvived {
			return nil
		}
	default:
	}

	// The parent may already have exited while a worker in its process group is
	// still serving. Always signal the owned group; returning just because Wait
	// completed would leak exactly the orphan topology SC012 detects.
	if err := syscall.Kill(-c.pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill process group %d: %w", c.pgid, err)
	}

	// Reap, so the run never leaves a zombie behind.
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
		return fmt.Errorf("process group %d did not exit after SIGKILL", c.pgid)
	}
	return nil
}

func exitStatusFrom(state *os.ProcessState, waitErr error) ExitStatus {
	if state == nil {
		if waitErr != nil {
			return ExitStatus{}
		}
		return ExitStatus{}
	}

	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return ExitStatus{Known: true, Signaled: true, TerminatedBy: signalName(ws.Signal())}
	}
	return ExitStatus{Known: true, Code: state.ExitCode()}
}

// attachControl signals a process this tool did not spawn.
type attachControl struct {
	pid int
}

func newAttachControl(pid int) (processControl, error) {
	if err := syscall.Kill(pid, 0); err != nil {
		return nil, fmt.Errorf("cannot signal PID %d: %w", pid, err)
	}
	return &attachControl{pid: pid}, nil
}

func (a *attachControl) Start() error { return nil }
func (a *attachControl) PID() int     { return a.pid }

func (a *attachControl) SignalPID(sig Signal) error {
	osSig, err := osSignal(sig)
	if err != nil {
		return err
	}
	if err := syscall.Kill(a.pid, osSig); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return fmt.Errorf("signal PID %d: %w", a.pid, err)
	}
	return nil
}

func (a *attachControl) Alive() (bool, error) {
	err := syscall.Kill(a.pid, 0)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	case errors.Is(err, syscall.EPERM):
		// Running, but owned by another user.
		return true, nil
	default:
		return false, err
	}
}

// Wait polls, because a process that is not our child cannot be waited on.
// The exit status is therefore unknown, and is reported as such rather than
// being invented.

func (a *attachControl) Wait(ctx context.Context) (ExitStatus, error) {
	ticker := time.NewTicker(attachPollInterval)
	defer ticker.Stop()
	for {
		alive, err := a.Alive()
		if err != nil {
			return ExitStatus{}, err
		}
		if !alive {
			return ExitStatus{Known: false}, nil
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return ExitStatus{}, ctx.Err()
		}
	}
}

// Cleanup deliberately does nothing: the tool did not start this process and
// must not destroy it on the way out. Escalation to SIGKILL is the run's
// explicit decision, not a side effect of tidying up.
func (a *attachControl) Cleanup() error { return nil }
