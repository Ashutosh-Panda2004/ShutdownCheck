package target

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/shutdowncheck/shutdowncheck/internal/clock"
	"github.com/shutdowncheck/shutdowncheck/internal/testutil"
)

// processTarget holds the portable half of target management: lifecycle
// ordering, readiness gating, idempotent cleanup. Driving it through a fake
// control exercises that logic on every platform, including Windows where no
// real process target can be constructed at all.

type fakeControl struct {
	mu sync.Mutex

	startErr  error
	signalErr error
	pid       int
	alive     bool
	aliveErr  error
	signals   []Signal
	status    ExitStatus
	cleanups  int
	started   bool
	exitAfter time.Duration
	deadline  *time.Time
}

func (f *fakeControl) Start() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.startErr != nil {
		return f.startErr
	}
	f.started = true
	f.alive = true
	if f.exitAfter > 0 {
		at := time.Now().Add(f.exitAfter)
		f.deadline = &at
	}
	return nil
}

func (f *fakeControl) PID() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pid
}

func (f *fakeControl) SignalPID(sig Signal) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.signals = append(f.signals, sig)
	if f.signalErr != nil {
		return f.signalErr
	}
	f.alive = false
	return nil
}

func (f *fakeControl) Alive() (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.aliveErr != nil {
		return false, f.aliveErr
	}
	if f.deadline != nil && time.Now().After(*f.deadline) {
		f.alive = false
	}
	return f.alive, nil
}

func (f *fakeControl) Wait(ctx context.Context) (ExitStatus, error) {
	for {
		alive, err := f.Alive()
		if err != nil {
			return ExitStatus{}, err
		}
		if !alive {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.status, nil
		}
		select {
		case <-time.After(2 * time.Millisecond):
		case <-ctx.Done():
			return ExitStatus{}, ctx.Err()
		}
	}
}

func (f *fakeControl) Cleanup() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleanups++
	f.alive = false
	return nil
}

func (f *fakeControl) sentSignals() []Signal {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Signal(nil), f.signals...)
}

func (f *fakeControl) cleanupCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cleanups
}

func newFakeTarget(control processControl, ready ReadyCheck) *processTarget {
	return &processTarget{
		desc:    Descriptor{Label: "fake"},
		control: control,
		ready:   ready,
		clk:     clock.System(),
		spawned: true,
	}
}

func TestProcessTargetLifecycle(t *testing.T) {
	testutil.NoLeaks(t)

	control := &fakeControl{pid: 1234, status: ExitStatus{Known: true, Code: 0}}
	tgt := newFakeTarget(control, ReadyCheck{})

	if err := tgt.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := tgt.Describe().PID; got != 1234 {
		t.Errorf("PID = %d, want 1234", got)
	}

	if alive, err := tgt.Alive(); err != nil || !alive {
		t.Fatalf("Alive() = %v, %v; want true", alive, err)
	}

	if err := tgt.Signal(SIGTERM); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	if got := control.sentSignals(); len(got) != 1 || got[0] != SIGTERM {
		t.Errorf("signals = %v, want [TERM]", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	status, err := tgt.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !status.Known || status.Code != 0 {
		t.Errorf("status = %s, want exit 0", status)
	}
}

func TestProcessTargetRejectsSecondStart(t *testing.T) {
	tgt := newFakeTarget(&fakeControl{}, ReadyCheck{})

	if err := tgt.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := tgt.Start(context.Background()); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("second Start = %v, want ErrAlreadyStarted", err)
	}
}

func TestProcessTargetSignalBeforeStart(t *testing.T) {
	tgt := newFakeTarget(&fakeControl{}, ReadyCheck{})

	if err := tgt.Signal(SIGTERM); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("Signal before Start = %v, want ErrNotStarted", err)
	}
}

func TestProcessTargetRejectsUnknownSignal(t *testing.T) {
	tgt := newFakeTarget(&fakeControl{}, ReadyCheck{})
	if err := tgt.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := tgt.Signal(Signal("HUP")); err == nil {
		t.Fatal("an unsupported signal should be rejected")
	}
}

// Leaving a spawned process running after a failed start would poison every
// later run on the same machine.
func TestProcessTargetClosesWhenReadinessFails(t *testing.T) {
	testutil.NoLeaks(t)

	control := &fakeControl{}
	tgt := newFakeTarget(control, ReadyCheck{
		Addr:     "127.0.0.1:1",
		Timeout:  100 * time.Millisecond,
		Interval: 10 * time.Millisecond,
	})

	if err := tgt.Start(context.Background()); !IsReadyTimeout(err) {
		t.Fatalf("Start = %v, want a readiness timeout", err)
	}
	if control.cleanupCount() == 0 {
		t.Error("a target that never became ready must be cleaned up")
	}
}

func TestProcessTargetCloseIsIdempotent(t *testing.T) {
	control := &fakeControl{}
	tgt := newFakeTarget(control, ReadyCheck{})

	for range 3 {
		if err := tgt.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
	if got := control.cleanupCount(); got != 1 {
		t.Errorf("cleanup ran %d times, want exactly 1", got)
	}
}

func TestProcessTargetStartPropagatesControlFailure(t *testing.T) {
	control := &fakeControl{startErr: errors.New("exec failed")}
	tgt := newFakeTarget(control, ReadyCheck{})

	if err := tgt.Start(context.Background()); err == nil {
		t.Fatal("Start should surface the control's failure")
	}
}

func TestProcessTargetWaitHonoursContext(t *testing.T) {
	testutil.NoLeaks(t)

	control := &fakeControl{}
	tgt := newFakeTarget(control, ReadyCheck{})
	if err := tgt.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := tgt.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait = %v, want a deadline error", err)
	}
	_ = tgt.Close()
}

func TestProcessTargetGracePeriodFallsBackToPlatformDefault(t *testing.T) {
	tgt := newFakeTarget(&fakeControl{}, ReadyCheck{})
	if got := tgt.DefaultGracePeriod(); got <= 0 {
		t.Fatalf("DefaultGracePeriod() = %v, want a positive default", got)
	}

	tgt.grace = 12 * time.Second
	if got := tgt.DefaultGracePeriod(); got != 12*time.Second {
		t.Errorf("DefaultGracePeriod() = %v, want the configured 12s", got)
	}
}

func TestLogWritersDisabledWithoutSink(t *testing.T) {
	stdout, stderr := logWriters(CommandOptions{})
	if stdout != nil || stderr != nil {
		t.Fatal("capture writers should be nil when no sink is configured")
	}

	stdout, stderr = logWriters(CommandOptions{LogSink: func(string, string) {}})
	if stdout == nil || stderr == nil {
		t.Fatal("both streams need a capture writer when a sink is configured")
	}
}
