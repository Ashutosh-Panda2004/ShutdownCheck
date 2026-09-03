package run

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shutdowncheck/shutdowncheck/internal/analyze"
	"github.com/shutdowncheck/shutdowncheck/internal/clock"
	"github.com/shutdowncheck/shutdowncheck/internal/load"
	"github.com/shutdowncheck/shutdowncheck/internal/target"
	"github.com/shutdowncheck/shutdowncheck/internal/testutil"
	"github.com/shutdowncheck/shutdowncheck/internal/timeline"
)

// fakeTarget models a process's response to signals without needing one.
type fakeTarget struct {
	mu sync.Mutex

	startErr   error
	signalErr  error
	ignoreTerm bool
	drainFor   time.Duration

	signals  []target.Signal
	deadline *time.Time
	closes   int
	status   target.ExitStatus
}

func (f *fakeTarget) Describe() target.Descriptor {
	return target.Descriptor{Kind: analyze.TargetCommand, Label: "fake", PID: 4242}
}

func (f *fakeTarget) Start(context.Context) error { return f.startErr }

func (f *fakeTarget) Signal(sig target.Signal) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.signals = append(f.signals, sig)
	if f.signalErr != nil {
		return f.signalErr
	}

	switch {
	case sig == target.SIGKILL:
		now := time.Now()
		f.deadline = &now
		f.status = target.ExitStatus{Known: true, Signaled: true, TerminatedBy: target.SIGKILL}
	case !f.ignoreTerm:
		at := time.Now().Add(f.drainFor)
		f.deadline = &at
		f.status = target.ExitStatus{Known: true, Code: 0}
	}
	return nil
}

func (f *fakeTarget) Alive() (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.deadline == nil {
		return true, nil
	}
	return time.Now().Before(*f.deadline), nil
}

func (f *fakeTarget) Wait(ctx context.Context) (target.ExitStatus, error) {
	for {
		alive, _ := f.Alive()
		if !alive {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.status, nil
		}
		select {
		case <-time.After(5 * time.Millisecond):
		case <-ctx.Done():
			return target.ExitStatus{}, ctx.Err()
		}
	}
}

func (f *fakeTarget) DefaultGracePeriod() time.Duration { return time.Second }

func (f *fakeTarget) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	return nil
}

func (f *fakeTarget) sentSignals() []target.Signal {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]target.Signal(nil), f.signals...)
}

func (f *fakeTarget) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closes
}

// fakeGenerator records the phases it was asked to run.
type fakeGenerator struct {
	mu      sync.Mutex
	phases  []load.Phase
	latency time.Duration
}

func (g *fakeGenerator) Run(ctx context.Context, _ time.Time, phase load.Phase) load.Result {
	g.mu.Lock()
	g.phases = append(g.phases, phase)
	g.mu.Unlock()

	latency := g.latency
	if latency == 0 {
		latency = 20 * time.Millisecond
	}

	samples := make([]time.Duration, 30)
	for i := range samples {
		samples[i] = latency
	}

	select {
	case <-time.After(phase.Duration):
	case <-ctx.Done():
	}
	return load.Result{Dispatched: 30, Completed: 30, Latencies: samples}
}

func (g *fakeGenerator) recorded() []load.Phase {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]load.Phase(nil), g.phases...)
}

// fakePoller emits a fixed event each tick.
type fakePoller struct {
	build func() timeline.Event
	mu    sync.Mutex
	calls int
}

func (p *fakePoller) Poll(context.Context) timeline.Event {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return p.build()
}

func (p *fakePoller) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func testPolicy(t *testing.T, grace time.Duration) analyze.Policy {
	t.Helper()

	policy, err := analyze.PolicyFor(analyze.ProfileStandalone)
	if err != nil {
		t.Fatalf("PolicyFor: %v", err)
	}
	policy.GracePeriod = grace
	return policy
}

func baseOptions(t *testing.T, tgt target.Target, gen Generator, grace time.Duration) Options {
	t.Helper()

	return Options{
		Target:         tgt,
		Generator:      gen,
		Recorder:       timeline.NewRecorder(timeline.Meta{}, 0),
		Clock:          clock.System(),
		Policy:         testPolicy(t, grace),
		Warmup:         40 * time.Millisecond,
		Steady:         40 * time.Millisecond,
		Signal:         target.SIGTERM,
		EnforceSigkill: true,
		TargetInFlight: 5,
		MaxRPS:         2000,
		ConcurrencyCap: 512,
	}
}

func stagesOf(tl timeline.Timeline) []timeline.Stage {
	var out []timeline.Stage
	for _, e := range tl.EventsOfKind(timeline.KindStage) {
		out = append(out, e.Stage.Stage)
	}
	return out
}

func hasStage(tl timeline.Timeline, want timeline.Stage) bool {
	for _, s := range stagesOf(tl) {
		if s == want {
			return true
		}
	}
	return false
}

func TestRunHappyPath(t *testing.T) {
	testutil.NoLeaks(t)

	tgt := &fakeTarget{drainFor: 30 * time.Millisecond}
	gen := &fakeGenerator{}

	runner, err := New(baseOptions(t, tgt, gen, 300*time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := tgt.sentSignals(); len(got) != 1 || got[0] != target.SIGTERM {
		t.Fatalf("signals = %v, want exactly one SIGTERM", got)
	}
	if result.Killed {
		t.Error("a target that drained in time must not be killed")
	}
	if !result.ExitStatus.Known || result.ExitStatus.Code != 0 {
		t.Errorf("exit status = %s, want exit 0", result.ExitStatus)
	}

	for _, want := range []timeline.Stage{
		timeline.StagePreflight, timeline.StageStartTarget, timeline.StageWaitReady,
		timeline.StageWarmup, timeline.StageCalibrate, timeline.StageSteady,
		timeline.StageSignal, timeline.StageObserve, timeline.StagePostExit, timeline.StageComplete,
	} {
		if !hasStage(result.Timeline, want) {
			t.Errorf("stage %q was never recorded (got %v)", want, stagesOf(result.Timeline))
		}
	}

	if _, ok := result.Timeline.SignalOffset(); !ok {
		t.Error("the signal was not recorded on the timeline")
	}
	if _, _, ok := result.Timeline.ProcessExit(); !ok {
		t.Error("process exit was not recorded")
	}
	if tgt.closeCount() == 0 {
		t.Error("the target was never closed")
	}
}

// The whole experiment turns on requests being in flight at the signal, so
// traffic has to run as one continuous phase spanning steady state and the
// entire grace budget. A gap there would leave the critical instant unobserved.
func TestRunTrafficSpansTheSignal(t *testing.T) {
	testutil.NoLeaks(t)

	const grace = 300 * time.Millisecond
	tgt := &fakeTarget{drainFor: 50 * time.Millisecond}
	gen := &fakeGenerator{}

	opts := baseOptions(t, tgt, gen, grace)
	runner, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := runner.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	phases := gen.recorded()
	if len(phases) != 2 {
		t.Fatalf("generator ran %d phases, want 2 (warmup then main)", len(phases))
	}
	if !phases[0].Warmup {
		t.Error("the first phase should be the warmup")
	}
	if phases[1].Warmup {
		t.Error("the main phase must not be marked as warmup")
	}

	wantAtLeast := opts.Steady + grace
	if phases[1].Duration < wantAtLeast {
		t.Fatalf("main phase lasted %v, want at least %v so traffic covers the whole grace budget",
			phases[1].Duration, wantAtLeast)
	}
}

// A target that ignores SIGTERM is the SC001/SC002 case. The run must escalate
// at grace expiry exactly as a real orchestrator would, rather than waiting
// indefinitely.
func TestRunEscalatesToSigkillAtGraceExpiry(t *testing.T) {
	testutil.NoLeaks(t)

	tgt := &fakeTarget{ignoreTerm: true}
	gen := &fakeGenerator{}

	runner, err := New(baseOptions(t, tgt, gen, 120*time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := tgt.sentSignals()
	if len(got) != 2 || got[0] != target.SIGTERM || got[1] != target.SIGKILL {
		t.Fatalf("signals = %v, want SIGTERM then SIGKILL", got)
	}
	if !result.Killed {
		t.Error("result should record that the target had to be killed")
	}
	if !hasStage(result.Timeline, timeline.StageSigkill) {
		t.Error("the sigkill stage was not recorded")
	}

	killAt, ok := result.Timeline.KillOffset()
	if !ok {
		t.Fatal("the SIGKILL was not recorded on the timeline")
	}
	signalAt, _ := result.Timeline.SignalOffset()
	if gap := killAt - signalAt; gap < 100*time.Millisecond {
		t.Errorf("SIGKILL landed %v after SIGTERM, want roughly the 120ms grace period", gap)
	}
}

func TestRunWithoutSigkillEnforcementWarnsInstead(t *testing.T) {
	testutil.NoLeaks(t)

	tgt := &fakeTarget{ignoreTerm: true}
	opts := baseOptions(t, tgt, &fakeGenerator{}, 100*time.Millisecond)
	opts.EnforceSigkill = false

	runner, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, sig := range tgt.sentSignals() {
		if sig == target.SIGKILL {
			t.Fatal("SIGKILL was sent despite enforcement being disabled")
		}
	}

	var warned bool
	for _, e := range result.Timeline.EventsOfKind(timeline.KindNotice) {
		if e.Notice.Code == "grace_exceeded" {
			warned = true
		}
	}
	if !warned {
		t.Error("exceeding the grace period without enforcement should still be reported")
	}
}

func TestRunRecordsObserverSamples(t *testing.T) {
	testutil.NoLeaks(t)

	readiness := &fakePoller{build: func() timeline.Event {
		return timeline.ReadinessAt(0, timeline.ReadinessEvent{Status: 200, Healthy: true})
	}}
	listener := &fakePoller{build: func() timeline.Event {
		return timeline.ListenerAt(0, timeline.ListenerEvent{Accepting: true})
	}}

	opts := baseOptions(t, &fakeTarget{drainFor: 30 * time.Millisecond}, &fakeGenerator{}, 200*time.Millisecond)
	opts.Readiness = readiness
	opts.Listener = listener
	opts.ObserveInterval = 10 * time.Millisecond

	runner, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if readiness.count() == 0 || listener.count() == 0 {
		t.Fatalf("observers polled %d and %d times, want both to have run",
			readiness.count(), listener.count())
	}
	if len(result.Timeline.EventsOfKind(timeline.KindReadiness)) == 0 {
		t.Error("no readiness samples reached the timeline")
	}
	if len(result.Timeline.EventsOfKind(timeline.KindListener)) == 0 {
		t.Error("no listener samples reached the timeline")
	}
}

// A port still accepting after the process has gone means an orphaned child
// kept the listener, which is what SC012 reports.
func TestRunRecordsPortReleaseAfterExit(t *testing.T) {
	testutil.NoLeaks(t)

	opts := baseOptions(t, &fakeTarget{drainFor: 20 * time.Millisecond}, &fakeGenerator{}, 200*time.Millisecond)
	opts.Listener = &fakePoller{build: func() timeline.Event {
		return timeline.ListenerAt(0, timeline.ListenerEvent{Accepting: false, Outcome: timeline.OutcomeRefused})
	}}
	opts.ObserveInterval = 20 * time.Millisecond

	runner, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var released bool
	for _, e := range result.Timeline.EventsOfKind(timeline.KindProcess) {
		if e.Process.Phase == timeline.ProcPortReleased {
			released = true
		}
	}
	if !released {
		t.Error("a refused post-exit probe should record the port as released")
	}
}

func TestRunFailsWhenTargetWillNotStart(t *testing.T) {
	testutil.NoLeaks(t)

	tgt := &fakeTarget{startErr: errors.New("boom")}
	runner, err := New(baseOptions(t, tgt, &fakeGenerator{}, time.Second))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := runner.Run(context.Background()); err == nil {
		t.Fatal("Run should fail when the target cannot start")
	}
	if tgt.closeCount() == 0 {
		t.Error("the target must still be closed after a failed start")
	}
}

func TestRunFailsWhenSignalCannotBeDelivered(t *testing.T) {
	testutil.NoLeaks(t)

	tgt := &fakeTarget{signalErr: errors.New("permission denied")}
	runner, err := New(baseOptions(t, tgt, &fakeGenerator{}, 200*time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = runner.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("Run error = %v, want the delivery failure surfaced", err)
	}
}

func TestRunStopsOnContextCancellation(t *testing.T) {
	testutil.NoLeaks(t)

	opts := baseOptions(t, &fakeTarget{ignoreTerm: true}, &fakeGenerator{}, 10*time.Second)
	runner, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	if _, err := runner.Run(ctx); err == nil {
		t.Log("run returned without error after cancellation, which is acceptable")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("cancellation took %v to take effect", elapsed)
	}
}

func TestRunUsesFixedRateWhenCalibrationIsDisabled(t *testing.T) {
	testutil.NoLeaks(t)

	gen := &fakeGenerator{}
	opts := baseOptions(t, &fakeTarget{drainFor: 20 * time.Millisecond}, gen, 150*time.Millisecond)
	opts.FixedRate = 123

	runner, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if result.Calibrated {
		t.Error("an explicit rate must not be reported as calibrated")
	}

	phases := gen.recorded()
	if len(phases) != 1 {
		t.Fatalf("ran %d phases, want 1 (warmup is skipped without calibration)", len(phases))
	}
	if phases[0].Rate != 123 {
		t.Errorf("rate = %v, want the explicit 123", phases[0].Rate)
	}
}

// Reporting an unreachable in-flight goal is what stops a fast endpoint
// producing a meaningless pass.
func TestRunReportsUnreachableInFlightGoal(t *testing.T) {
	testutil.NoLeaks(t)

	gen := &fakeGenerator{latency: time.Microsecond}
	opts := baseOptions(t, &fakeTarget{drainFor: 20 * time.Millisecond}, gen, 150*time.Millisecond)
	opts.TargetInFlight = 5000
	opts.MaxRPS = 100

	runner, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if result.Calibration.Achievable {
		t.Fatal("the goal was unreachable and should be reported as such")
	}

	var flagged bool
	for _, e := range result.Timeline.EventsOfKind(timeline.KindNotice) {
		if e.Notice.Code == string(analyze.SC000) {
			flagged = true
		}
	}
	if !flagged {
		t.Error("an unreachable in-flight goal must be recorded as SC000 on the timeline")
	}
}

func TestNewValidatesOptions(t *testing.T) {
	valid := baseOptions(t, &fakeTarget{}, &fakeGenerator{}, time.Second)

	cases := map[string]func(*Options){
		"no target":    func(o *Options) { o.Target = nil },
		"no generator": func(o *Options) { o.Generator = nil },
		"no recorder":  func(o *Options) { o.Recorder = nil },
		// SIGKILL destroys rather than drains; using it would make the whole
		// measurement meaningless.
		"kill signal": func(o *Options) { o.Signal = target.SIGKILL },
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			opts := valid
			mutate(&opts)
			if _, err := New(opts); err == nil {
				t.Fatalf("New should have rejected: %s", name)
			}
		})
	}
}

func TestNewDefaultsSignalToTerm(t *testing.T) {
	opts := baseOptions(t, &fakeTarget{}, &fakeGenerator{}, time.Second)
	opts.Signal = ""

	runner, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if runner.opts.Signal != target.SIGTERM {
		t.Errorf("default signal = %q, want TERM", runner.opts.Signal)
	}
}
