package run

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/analyze"
	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/clock"
	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/load"
	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/target"
	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/testutil"
	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/timeline"
)

// fakeTarget models a process's response to signals without needing one.
type fakeTarget struct {
	mu sync.Mutex

	startErr   error
	signalErr  error
	killErr    error
	aliveErr   error
	waitErr    error
	closeErr   error
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
	if sig == target.SIGKILL && f.killErr != nil {
		return f.killErr
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
	if f.aliveErr != nil {
		return false, f.aliveErr
	}

	if f.deadline == nil {
		return true, nil
	}
	return time.Now().Before(*f.deadline), nil
}

func (f *fakeTarget) Wait(ctx context.Context) (target.ExitStatus, error) {
	if f.waitErr != nil {
		return target.ExitStatus{}, f.waitErr
	}
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
	return f.closeErr
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
	mu          sync.Mutex
	phases      []load.Phase
	latency     time.Duration
	pinnedDelay time.Duration
}

func (g *fakeGenerator) Run(ctx context.Context, _ time.Time, phase load.Phase) load.Result {
	g.mu.Lock()
	g.phases = append(g.phases, phase)
	g.mu.Unlock()

	latency := g.latency
	if latency == 0 {
		latency = 20 * time.Millisecond
	}
	if phase.PinnedReady != nil {
		if g.pinnedDelay > 0 {
			time.Sleep(g.pinnedDelay)
		}
		phase.PinnedReady <- phase.PinnedCount
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

type canceledPoller struct {
	started chan struct{}
	event   timeline.Event
}

func (p *canceledPoller) Poll(ctx context.Context) timeline.Event {
	close(p.started)
	<-ctx.Done()
	return p.event
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

func TestSlowURLIsPinnedAcrossTheSignalPhase(t *testing.T) {
	tgt := &fakeTarget{drainFor: 10 * time.Millisecond}
	gen := &fakeGenerator{}
	opts := baseOptions(t, tgt, gen, 200*time.Millisecond)
	opts.TargetInFlight = 2
	opts.SlowURL = "http://localhost:8080/slow"

	runner, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := runner.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	phases := gen.recorded()
	if len(phases) != 2 {
		t.Fatalf("generator ran %d phases, want warmup and termination phases", len(phases))
	}
	phase := phases[1]
	if phase.PinnedRequest == nil || phase.PinnedRequest.URL != opts.SlowURL {
		t.Fatalf("pinned request = %+v", phase.PinnedRequest)
	}
	if phase.PinnedCount != opts.Policy.MinInFlightSample {
		t.Errorf("PinnedCount = %d, want minimum sample %d", phase.PinnedCount, opts.Policy.MinInFlightSample)
	}
	if phase.PinnedAt != opts.Steady+opts.PreStopSleep {
		t.Errorf("PinnedAt = %s, want the signal boundary %s", phase.PinnedAt, opts.Steady+opts.PreStopSleep)
	}
}

func TestPinnedDispatchDelayIsRecordedAsSignalSkew(t *testing.T) {
	testutil.NoLeaks(t)

	gen := &fakeGenerator{pinnedDelay: 120 * time.Millisecond}
	opts := baseOptions(t, &fakeTarget{drainFor: 10 * time.Millisecond}, gen, 150*time.Millisecond)
	opts.TargetInFlight = 5
	opts.SlowURL = "http://localhost:8080/slow"

	runner, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	signal, _, ok := result.Timeline.TerminationSignal()
	if !ok || signal.Skew <= analyze.MaxSignalSkew {
		t.Fatalf("signal skew = %s, want above %s", signal.Skew, analyze.MaxSignalSkew)
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

func TestRunPrimesObserversBeforeTraffic(t *testing.T) {
	testutil.NoLeaks(t)

	readiness := &fakePoller{build: func() timeline.Event {
		return timeline.ReadinessAt(0, timeline.ReadinessEvent{Status: 200, Healthy: true, Outcome: timeline.OutcomeOK})
	}}
	listener := &fakePoller{build: func() timeline.Event {
		return timeline.ListenerAt(0, timeline.ListenerEvent{Accepting: true, Outcome: timeline.OutcomeOK})
	}}
	opts := baseOptions(t, &fakeTarget{drainFor: time.Millisecond}, &fakeGenerator{}, 2*time.Millisecond)
	opts.Readiness = readiness
	opts.Listener = listener
	opts.Warmup = time.Nanosecond
	opts.Steady = time.Nanosecond
	opts.ObserveInterval = time.Hour

	runner, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Timeline.EventsOfKind(timeline.KindReadiness)) == 0 ||
		len(result.Timeline.EventsOfKind(timeline.KindListener)) == 0 {
		t.Fatal("observers were not sampled before the short run completed")
	}
}

func TestStoppingObserversDoesNotRecordCanceledPolls(t *testing.T) {
	for _, test := range []struct {
		name  string
		kind  timeline.Kind
		event timeline.Event
	}{
		{
			name: "readiness", kind: timeline.KindReadiness,
			event: timeline.ReadinessAt(0, timeline.ReadinessEvent{
				Healthy: false, Outcome: timeline.OutcomeAbandoned,
			}),
		},
		{
			name: "listener", kind: timeline.KindListener,
			event: timeline.ListenerAt(0, timeline.ListenerEvent{
				Accepting: false, Outcome: timeline.OutcomeAbandoned,
			}),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			testutil.NoLeaks(t)
			recorder := timeline.NewRecorder(timeline.Meta{}, 100)
			poller := &canceledPoller{started: make(chan struct{}), event: test.event}
			runner := &Runner{
				opts: Options{Recorder: recorder, Readiness: poller, ObserveInterval: time.Millisecond},
				clk:  clock.System(),
			}
			if test.kind == timeline.KindListener {
				runner.opts.Readiness = nil
				runner.opts.Listener = poller
			}

			stop := runner.startObservers(context.Background())
			select {
			case <-poller.started:
			case <-time.After(time.Second):
				t.Fatal("observer poll did not start")
			}
			stop()

			if got := len(recorder.Snapshot().EventsOfKind(test.kind)); got != 0 {
				t.Fatalf("recorded %d canceled %s poll(s), want 0", got, test.name)
			}
		})
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
	if _, err := runner.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context cancellation", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("cancellation took %v to take effect", elapsed)
	}
}

func TestRunPropagatesLivenessFailure(t *testing.T) {
	tgt := &fakeTarget{aliveErr: errors.New("cannot observe process")}
	opts := baseOptions(t, tgt, &fakeGenerator{}, 100*time.Millisecond)
	opts.FixedRate = 10

	runner, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := runner.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "cannot observe") {
		t.Fatalf("Run error = %v", err)
	}
}

func TestRunPropagatesFailedSigkill(t *testing.T) {
	tgt := &fakeTarget{ignoreTerm: true, killErr: errors.New("kill denied")}
	opts := baseOptions(t, tgt, &fakeGenerator{}, 50*time.Millisecond)
	opts.FixedRate = 10

	runner, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := runner.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "kill denied") {
		t.Fatalf("Run error = %v", err)
	}
	if result.Killed {
		t.Error("result says the target was killed even though SIGKILL failed")
	}
}

func TestRunPropagatesExitObservationFailure(t *testing.T) {
	tgt := &fakeTarget{waitErr: errors.New("wait failed")}
	opts := baseOptions(t, tgt, &fakeGenerator{}, 100*time.Millisecond)
	opts.FixedRate = 10

	runner, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := runner.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "wait failed") {
		t.Fatalf("Run error = %v", err)
	}
}

func TestRunPropagatesCleanupFailureAndRecordsIt(t *testing.T) {
	tgt := &fakeTarget{drainFor: 10 * time.Millisecond, closeErr: errors.New("cleanup failed")}
	opts := baseOptions(t, tgt, &fakeGenerator{}, 100*time.Millisecond)
	opts.FixedRate = 10

	runner, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := runner.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cleanup failed") {
		t.Fatalf("Run error = %v", err)
	}

	for _, event := range result.Timeline.EventsOfKind(timeline.KindNotice) {
		if event.Notice != nil && event.Notice.Code == "cleanup_failed" {
			return
		}
	}
	t.Fatal("cleanup failure was not retained in the final timeline")
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
	if len(phases) != 2 {
		t.Fatalf("ran %d phases, want 2 (warmup then fixed-rate traffic)", len(phases))
	}
	if !phases[0].Warmup || phases[0].Rate != WarmupRate {
		t.Errorf("warmup phase = %+v, want %v rps warmup", phases[0], WarmupRate)
	}
	if phases[1].Rate != 123 {
		t.Errorf("main rate = %v, want the explicit 123", phases[1].Rate)
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
