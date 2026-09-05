package target

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/shutdowncheck/shutdowncheck/internal/analyze"
	"github.com/shutdowncheck/shutdowncheck/internal/clock"
	"github.com/shutdowncheck/shutdowncheck/internal/redact"
)

// processControl is the whole platform-specific surface of this package.
//
// Everything above it — lifecycle, readiness, log capture, cleanup ordering —
// is portable and testable anywhere. Keeping the seam this narrow is deliberate:
// signal delivery cannot be exercised on Windows at all, so the less that sits
// below the line, the less goes unverified there.
type processControl interface {
	Start() error
	PID() int
	SignalPID(sig Signal) error
	Alive() (bool, error)
	Wait(ctx context.Context) (ExitStatus, error)
	Cleanup() error
}

// CommandOptions configures a target that shutdowncheck spawns and owns.
type CommandOptions struct {
	// Argv is the command and its arguments. It is executed directly; no shell
	// is involved and no string is ever interpolated into a command line.
	Argv []string
	Dir  string
	Env  []string

	Ready       ReadyCheck
	LogSink     LogSink
	MaxLogBytes int64
	GracePeriod time.Duration
	Clock       clock.Clock
}

// ProcessOptions configures a target attached to an existing process.
type ProcessOptions struct {
	PID         int
	Ready       ReadyCheck
	GracePeriod time.Duration
	Clock       clock.Clock
	// AllowUnsafePID permits attaching to PID 1, which is a real target inside
	// a container and a catastrophe on a host.
	AllowUnsafePID bool
}

// NewCommand returns a target that spawns and owns a process group.
func NewCommand(opts CommandOptions) (Target, error) {
	if len(opts.Argv) == 0 || strings.TrimSpace(opts.Argv[0]) == "" {
		return nil, &UnsafeTargetError{Reason: "no command was given"}
	}

	stdout, stderr := logWriters(opts)
	control, err := newCommandControl(opts, stdout, stderr)
	if err != nil {
		return nil, err
	}

	return &processTarget{
		desc: Descriptor{
			Kind:  analyze.TargetCommand,
			Label: redact.Argv(opts.Argv),
		},
		control: control,
		ready:   opts.Ready,
		grace:   opts.GracePeriod,
		clk:     orSystemClock(opts.Clock),
		spawned: true,
	}, nil
}

// logWriters returns capture writers, or nil when capture is disabled so that
// exec can connect the child straight to the null device.
func logWriters(opts CommandOptions) (stdout, stderr io.Writer) {
	if opts.LogSink == nil {
		return nil, nil
	}
	return NewLogWriter("stdout", opts.MaxLogBytes, opts.LogSink),
		NewLogWriter("stderr", opts.MaxLogBytes, opts.LogSink)
}

// NewProcess returns a target attached to an existing process by PID.
func NewProcess(opts ProcessOptions) (Target, error) {
	if err := ValidatePID(opts.PID, os.Getpid(), opts.AllowUnsafePID); err != nil {
		return nil, err
	}

	control, err := newAttachControl(opts.PID)
	if err != nil {
		return nil, err
	}

	return &processTarget{
		desc:    Descriptor{Kind: analyze.TargetProcess, Label: fmt.Sprintf("pid(%d)", opts.PID), PID: opts.PID},
		control: control,
		ready:   opts.Ready,
		grace:   opts.GracePeriod,
		clk:     orSystemClock(opts.Clock),
	}, nil
}

func orSystemClock(c clock.Clock) clock.Clock {
	if c == nil {
		return clock.System()
	}
	return c
}

type processTarget struct {
	desc    Descriptor
	control processControl
	ready   ReadyCheck
	grace   time.Duration
	clk     clock.Clock
	spawned bool

	mu        sync.Mutex
	started   bool
	closeOnce sync.Once
	closeErr  error
}

func (t *processTarget) Describe() Descriptor {
	t.mu.Lock()
	defer t.mu.Unlock()

	desc := t.desc
	if pid := t.control.PID(); pid != 0 {
		desc.PID = pid
	}
	return desc
}

func (t *processTarget) Start(ctx context.Context) error {
	t.mu.Lock()
	if t.started {
		t.mu.Unlock()
		return ErrAlreadyStarted
	}
	t.started = true
	t.mu.Unlock()

	if err := t.control.Start(); err != nil {
		return err
	}

	if err := WaitReady(ctx, t.ready, t.clk, t.control.Alive); err != nil {
		// A target that never came up must not be left running behind us.
		_ = t.Close()
		return err
	}
	return nil
}

func (t *processTarget) Signal(sig Signal) error {
	if !sig.Valid() {
		return fmt.Errorf("unsupported signal %q", sig)
	}

	t.mu.Lock()
	started := t.started
	t.mu.Unlock()
	if t.spawned && !started {
		return ErrNotStarted
	}

	return t.control.SignalPID(sig)
}

func (t *processTarget) Alive() (bool, error) { return t.control.Alive() }

func (t *processTarget) Wait(ctx context.Context) (ExitStatus, error) {
	return t.control.Wait(ctx)
}

func (t *processTarget) DefaultGracePeriod() time.Duration {
	if t.grace > 0 {
		return t.grace
	}
	return analyze.DefaultGracePeriod(t.desc.Kind)
}

// Close terminates anything still running and waits for log capture to finish.
//
// It is idempotent and must be safe to call from a deferred cleanup even if
// Start failed, because a spawned process group that outlives a failed run
// poisons every job that follows it on the same CI runner.
func (t *processTarget) Close() error {
	t.closeOnce.Do(func() {
		t.closeErr = t.control.Cleanup()
	})
	return t.closeErr
}
