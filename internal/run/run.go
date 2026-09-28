package run

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/analyze"
	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/clock"
	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/load"
	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/probe"
	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/target"
	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/timeline"
)

// Timing constants for the run state machine.
const (
	// WarmupRate is the fixed rate used before calibration. It only needs to
	// produce enough samples for a median, not to load the service.
	WarmupRate = 10.0

	// PostExitTail keeps traffic flowing briefly after the process has gone, so
	// the report can show connections being refused rather than merely stopping.
	PostExitTail = 750 * time.Millisecond

	// ExitPollInterval is how often the target is checked for having exited.
	ExitPollInterval = 10 * time.Millisecond
)

// Generator produces traffic. It is an interface so the orchestrator can be
// driven by a fake in tests.
type Generator interface {
	Run(ctx context.Context, origin time.Time, phase load.Phase) load.Result
}

// Poller performs one observation and returns the event to record.
type Poller interface {
	Poll(ctx context.Context) timeline.Event
}

// Options configures a run.
type Options struct {
	Target    target.Target
	Generator Generator
	Recorder  *timeline.Recorder
	Clock     clock.Clock
	Policy    analyze.Policy

	// Readiness and Listener are optional independent observers.
	Readiness Poller
	Listener  Poller
	// ObserveInterval is the cadence for both observers.
	ObserveInterval time.Duration

	Warmup         time.Duration
	Steady         time.Duration
	PreStopSleep   time.Duration
	Signal         target.Signal
	EnforceSigkill bool

	// Calibration inputs. When FixedRate is positive, calibration is skipped.
	FixedRate      float64
	TargetInFlight int
	MaxRPS         float64
	ConcurrencyCap int
	SlowURL        string
}

// Result is what a completed run produces.
type Result struct {
	Timeline    timeline.Timeline
	Calibration load.Calibration
	Calibrated  bool
	ExitStatus  target.ExitStatus
	Killed      bool
}

// Runner executes one experiment.
type Runner struct {
	opts   Options
	clk    clock.Clock
	origin time.Time
}

// New builds a runner.
func New(opts Options) (*Runner, error) {
	if opts.Target == nil {
		return nil, errors.New("run: a target is required")
	}
	if opts.Generator == nil {
		return nil, errors.New("run: a traffic generator is required")
	}
	if opts.Recorder == nil {
		return nil, errors.New("run: a recorder is required")
	}
	if opts.Signal == "" {
		opts.Signal = target.SIGTERM
	}
	if !opts.Signal.Graceful() {
		return nil, fmt.Errorf("run: %q is not a graceful signal; the experiment measures draining, not destruction", opts.Signal)
	}
	if opts.ObserveInterval <= 0 {
		opts.ObserveInterval = 20 * time.Millisecond
	}

	clk := opts.Clock
	if clk == nil {
		clk = clock.System()
	}
	return &Runner{opts: opts, clk: clk}, nil
}

// Run executes the full state machine and returns the recorded timeline.
//
// Cleanup is unconditional: whatever happens, the target is closed and the
// observers are stopped before returning. A spawned process group that survives
// a failed run would corrupt every later run on the same machine.
func (r *Runner) Run(ctx context.Context) (result Result, runErr error) {
	r.origin = r.clk.Now()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	defer func() {
		if closeErr := r.opts.Target.Close(); closeErr != nil {
			r.notice("error", "cleanup_failed", closeErr.Error())
			cleanupErr := fmt.Errorf("cleanup target: %w", closeErr)
			if runErr == nil {
				runErr = cleanupErr
			} else {
				runErr = errors.Join(runErr, cleanupErr)
			}
		}
		// Cleanup can emit target logs or a failure notice, so this must be the
		// final snapshot on every return path.
		result.Timeline = r.opts.Recorder.Snapshot()
	}()

	r.stage(timeline.StagePreflight)

	if err := r.startTarget(ctx); err != nil {
		return result, err
	}
	if err := r.primeObservers(ctx); err != nil {
		return result, err
	}

	stopObservers := r.startObservers(ctx)
	defer stopObservers()

	calibration, calibrated, err := r.warmupAndCalibrate(ctx)
	if err != nil {
		return result, err
	}

	result, runErr = r.terminate(ctx, calibration.RPS)
	result.Calibration = calibration
	result.Calibrated = calibrated

	stopObservers()
	r.stage(timeline.StageComplete)
	return result, runErr
}

func (r *Runner) primeObservers(ctx context.Context) error {
	for _, poller := range []Poller{r.opts.Readiness, r.opts.Listener} {
		if poller == nil {
			continue
		}
		event := poller.Poll(ctx)
		if err := ctx.Err(); err != nil {
			return err
		}
		r.record(event)
	}
	return nil
}

func (r *Runner) startTarget(ctx context.Context) error {
	r.stage(timeline.StageStartTarget)

	if err := r.opts.Target.Start(ctx); err != nil {
		return fmt.Errorf("start target: %w", err)
	}

	desc := r.opts.Target.Describe()
	r.record(timeline.ProcessAt(r.now(), timeline.ProcessEvent{Phase: timeline.ProcStarted, PID: desc.PID}))

	r.stage(timeline.StageWaitReady)
	r.record(timeline.ProcessAt(r.now(), timeline.ProcessEvent{Phase: timeline.ProcReady, PID: desc.PID}))
	return nil
}

func (r *Runner) warmupAndCalibrate(ctx context.Context) (load.Calibration, bool, error) {
	r.stage(timeline.StageWarmup)
	warmup := r.opts.Generator.Run(ctx, r.origin, load.Phase{
		Rate:     WarmupRate,
		Duration: r.opts.Warmup,
		Warmup:   true,
	})
	if err := ctx.Err(); err != nil {
		return load.Calibration{}, false, err
	}
	if r.opts.FixedRate > 0 {
		baseline, warnings, err := load.AssessBaseline(
			warmup.Latencies, warmup.Errors, warmup.Completed)
		if err != nil {
			return load.Calibration{}, false, fmt.Errorf("assess baseline: %w", err)
		}
		calibration := load.Calibration{
			RPS: r.opts.FixedRate, BaselineLatency: baseline,
			ExpectedInFlight: r.opts.FixedRate * baseline.Seconds(),
			Achievable:       true, Warnings: warnings,
		}
		for _, warning := range warnings {
			r.notice("warn", "baseline", warning)
		}
		r.notice("info", "fixed_rate", fmt.Sprintf(
			"baseline %s, fixed rate %.0f rps, expecting %.1f requests in flight",
			baseline, r.opts.FixedRate, calibration.ExpectedInFlight))
		return calibration, false, nil
	}

	r.stage(timeline.StageCalibrate)
	calibration, err := load.Calibrate(load.Input{
		Latencies:      warmup.Latencies,
		Errors:         warmup.Errors,
		Total:          warmup.Completed,
		TargetInFlight: r.opts.TargetInFlight,
		MaxRPS:         r.opts.MaxRPS,
		ConcurrencyCap: r.opts.ConcurrencyCap,
	})
	if err != nil {
		return load.Calibration{}, false, fmt.Errorf("calibrate: %w", err)
	}

	for _, warning := range calibration.Warnings {
		r.notice("warn", "calibration", warning)
	}
	if !calibration.Achievable {
		r.notice("warn", string(analyze.SC000),
			fmt.Sprintf("could not reach the in-flight goal of %d; this run cannot prove correct draining",
				r.opts.TargetInFlight))
	}
	r.notice("info", "calibrated", fmt.Sprintf(
		"baseline %s, rate %.0f rps, expecting %.1f requests in flight",
		calibration.BaselineLatency, calibration.RPS, calibration.ExpectedInFlight))

	return calibration, true, nil
}

// terminate runs traffic continuously across the signal.
//
// A single uninterrupted traffic phase spans steady state, the pre-stop hold and
// the whole grace budget. Splitting it around the signal would leave a gap
// exactly where requests need to be in flight, which is the one moment the
// experiment exists to observe.
func (r *Runner) terminate(ctx context.Context, rate float64) (Result, error) {
	grace := r.opts.Policy.GracePeriod
	total := r.opts.Steady + r.opts.PreStopSleep + grace + PostExitTail
	signalDue := r.now() + r.opts.Steady + r.opts.PreStopSleep

	trafficCtx, stopTraffic := context.WithCancel(ctx)
	defer stopTraffic()

	var traffic sync.WaitGroup
	var pinnedReady <-chan int
	pinnedWanted := 0
	traffic.Add(1)
	phase := load.Phase{Rate: rate, Duration: total}
	if r.opts.SlowURL != "" && r.opts.TargetInFlight > 0 {
		slow := probe.Request{Name: "slow", Method: "GET", URL: r.opts.SlowURL}
		ready := make(chan int, 1)
		phase.PinnedRequest = &slow
		phase.PinnedCount = r.opts.TargetInFlight
		if phase.PinnedCount < r.opts.Policy.MinInFlightSample {
			phase.PinnedCount = r.opts.Policy.MinInFlightSample
		}
		if phase.PinnedCount > r.opts.ConcurrencyCap {
			phase.PinnedCount = r.opts.ConcurrencyCap
		}
		phase.PinnedAt = r.opts.Steady + r.opts.PreStopSleep
		phase.PinnedReady = ready
		pinnedReady = ready
		pinnedWanted = phase.PinnedCount
	}
	go func() {
		defer traffic.Done()
		r.opts.Generator.Run(trafficCtx, r.origin, phase)
	}()

	r.stage(timeline.StageSteady)
	if !r.wait(ctx, r.opts.Steady) {
		stopTraffic()
		traffic.Wait()
		return Result{}, ctx.Err()
	}

	if r.opts.PreStopSleep > 0 {
		r.stage(timeline.StagePreStop)
		if !r.wait(ctx, r.opts.PreStopSleep) {
			stopTraffic()
			traffic.Wait()
			return Result{}, ctx.Err()
		}
	}

	if pinnedReady != nil {
		select {
		case started := <-pinnedReady:
			if started < pinnedWanted {
				r.notice("warn", string(analyze.SC000), fmt.Sprintf(
					"only %d of %d slow requests started before the signal", started, pinnedWanted))
			}
		case <-ctx.Done():
			stopTraffic()
			traffic.Wait()
			return Result{}, ctx.Err()
		}
	}

	r.stage(timeline.StageSignal)
	signalErr := r.opts.Target.Signal(r.opts.Signal)
	delivered := r.now()

	skew := delivered - signalDue
	if skew < 0 {
		skew = 0
	}
	r.record(timeline.SignalAt(delivered, timeline.SignalEvent{
		Signal: string(r.opts.Signal),
		Skew:   skew,
		Error:  errText(signalErr),
	}))
	if skew > analyze.MaxSignalSkew {
		r.notice("warn", "signal_skew", fmt.Sprintf(
			"signal delivery was %s late; the host may be too loaded for this run to be trusted", skew))
	}
	if signalErr != nil {
		stopTraffic()
		traffic.Wait()
		return Result{}, fmt.Errorf("deliver %s: %w", r.opts.Signal, signalErr)
	}

	r.stage(timeline.StageObserve)
	result, err := r.watchExit(ctx, delivered, grace)
	if err != nil {
		stopTraffic()
		traffic.Wait()
		return result, err
	}

	// Let traffic continue briefly past exit so refused connections are visible.
	if !r.wait(ctx, PostExitTail) {
		stopTraffic()
		traffic.Wait()
		return result, ctx.Err()
	}
	stopTraffic()
	traffic.Wait()

	r.postExitCheck(ctx)
	return result, nil
}

// watchExit waits for the process to go, escalating to SIGKILL at grace expiry.
func (r *Runner) watchExit(ctx context.Context, signalAt, grace time.Duration) (Result, error) {
	deadline := signalAt + grace
	var result Result

	for {
		alive, err := r.opts.Target.Alive()
		if err != nil {
			return result, fmt.Errorf("observe target liveness: %w", err)
		}
		if !alive {
			status, err := r.recordExit(ctx)
			result.ExitStatus = status
			return result, err
		}

		if r.now() >= deadline {
			break
		}
		if !r.wait(ctx, ExitPollInterval) {
			return result, ctx.Err()
		}
	}

	if !r.opts.EnforceSigkill {
		r.notice("warn", "grace_exceeded",
			"the target was still running at grace expiry; a real orchestrator would have killed it here")
		return result, nil
	}

	r.stage(timeline.StageSigkill)
	killAt := r.now()
	killErr := r.opts.Target.Signal(target.SIGKILL)
	r.record(timeline.SignalAt(killAt, timeline.SignalEvent{
		Signal: string(target.SIGKILL),
		Error:  errText(killErr),
	}))
	result.Killed = true
	if killErr != nil {
		result.Killed = false
		return result, fmt.Errorf("deliver %s: %w", target.SIGKILL, killErr)
	}

	status, err := r.recordExit(ctx)
	result.ExitStatus = status
	return result, err
}

func (r *Runner) recordExit(ctx context.Context) (target.ExitStatus, error) {
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	status, err := r.opts.Target.Wait(waitCtx)
	if err != nil {
		r.notice("warn", "exit_unobserved", err.Error())
		return status, fmt.Errorf("observe target exit: %w", err)
	}

	event := timeline.ProcessEvent{Phase: timeline.ProcExited, PID: r.opts.Target.Describe().PID}
	if status.Known {
		code := status.Code
		event.ExitCode = &code
		if status.Signaled {
			event.ExitCode = nil
			event.TerminatedBy = "SIG" + string(status.TerminatedBy)
		}
	}
	r.record(timeline.ProcessAt(r.now(), event))
	return status, nil
}

// postExitCheck looks for a port still bound after the process has gone, which
// means an orphaned child kept the listener and the next deploy will collide
// with it.
func (r *Runner) postExitCheck(ctx context.Context) {
	r.stage(timeline.StagePostExit)
	if r.opts.Listener == nil {
		return
	}

	event := r.opts.Listener.Poll(ctx)
	r.record(event)

	if event.Listener != nil && !event.Listener.Accepting {
		r.record(timeline.ProcessAt(r.now(), timeline.ProcessEvent{Phase: timeline.ProcPortReleased}))
	}
}

// startObservers runs the readiness and listener probes on their own cadence,
// independent of traffic. Sharing a schedule with the load generator would let a
// saturated pool delay them, and when readiness flips is the entire S2
// measurement.
func (r *Runner) startObservers(ctx context.Context) func() {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup

	for _, poller := range []Poller{r.opts.Readiness, r.opts.Listener} {
		if poller == nil {
			continue
		}
		wg.Add(1)
		go func(p Poller) {
			defer wg.Done()
			r.pollLoop(ctx, p)
		}(poller)
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			wg.Wait()
		})
	}
}

func (r *Runner) pollLoop(ctx context.Context, p Poller) {
	ticker := r.clk.NewTicker(r.opts.ObserveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			if ctx.Err() != nil {
				return
			}
			event := p.Poll(ctx)
			if ctx.Err() != nil {
				return
			}
			r.record(event)
		}
	}
}

// wait sleeps for d, reporting false if the context ended first.
func (r *Runner) wait(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	select {
	case <-r.clk.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

func (r *Runner) now() time.Duration { return r.clk.Since(r.origin) }

func (r *Runner) record(e timeline.Event) { r.opts.Recorder.Record(e) }

func (r *Runner) stage(s timeline.Stage) {
	r.record(timeline.StageAt(r.now(), s))
}

func (r *Runner) notice(level, code, message string) {
	r.record(timeline.NoticeAt(r.now(), level, code, message))
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
