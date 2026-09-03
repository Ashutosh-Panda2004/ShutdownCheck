package run

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/shutdowncheck/shutdowncheck/internal/analyze"
	"github.com/shutdowncheck/shutdowncheck/internal/clock"
	"github.com/shutdowncheck/shutdowncheck/internal/load"
	"github.com/shutdowncheck/shutdowncheck/internal/target"
	"github.com/shutdowncheck/shutdowncheck/internal/timeline"
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

	// SignalSkewBudget is the delivery lateness above which a run is flagged as
	// low confidence, because the host was too busy to time the experiment.
	SignalSkewBudget = 50 * time.Millisecond
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
func (r *Runner) Run(ctx context.Context) (Result, error) {
	r.origin = r.clk.Now()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	defer func() {
		if err := r.opts.Target.Close(); err != nil {
			r.notice("warn", "cleanup_failed", err.Error())
		}
	}()

	r.stage(timeline.StagePreflight)

	if err := r.startTarget(ctx); err != nil {
		return Result{Timeline: r.opts.Recorder.Snapshot()}, err
	}

	stopObservers := r.startObservers(ctx)
	defer stopObservers()

	calibration, calibrated, err := r.warmupAndCalibrate(ctx)
	if err != nil {
		return Result{Timeline: r.opts.Recorder.Snapshot()}, err
	}

	result, err := r.terminate(ctx, calibration.RPS)
	result.Calibration = calibration
	result.Calibrated = calibrated

	stopObservers()
	r.stage(timeline.StageComplete)
	result.Timeline = r.opts.Recorder.Snapshot()
	return result, err
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
	if r.opts.FixedRate > 0 {
		return load.Calibration{RPS: r.opts.FixedRate, Achievable: true}, false, nil
	}

	r.stage(timeline.StageWarmup)
	warmup := r.opts.Generator.Run(ctx, r.origin, load.Phase{
		Rate:     WarmupRate,
		Duration: r.opts.Warmup,
		Warmup:   true,
	})

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

	trafficCtx, stopTraffic := context.WithCancel(ctx)
	defer stopTraffic()

	var traffic sync.WaitGroup
	traffic.Add(1)
	go func() {
		defer traffic.Done()
		r.opts.Generator.Run(trafficCtx, r.origin, load.Phase{Rate: rate, Duration: total})
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

	intended := r.now()
	r.stage(timeline.StageSignal)
	signalErr := r.opts.Target.Signal(r.opts.Signal)
	delivered := r.now()

	skew := delivered - intended
	r.record(timeline.SignalAt(delivered, timeline.SignalEvent{
		Signal: string(r.opts.Signal),
		Skew:   skew,
		Error:  errText(signalErr),
	}))
	if skew > SignalSkewBudget {
		r.notice("warn", "signal_skew", fmt.Sprintf(
			"signal delivery was %s late; the host may be too loaded for this run to be trusted", skew))
	}
	if signalErr != nil {
		stopTraffic()
		traffic.Wait()
		return Result{}, fmt.Errorf("deliver %s: %w", r.opts.Signal, signalErr)
	}

	r.stage(timeline.StageObserve)
	result := r.watchExit(ctx, delivered, grace)

	// Let traffic continue briefly past exit so refused connections are visible.
	r.wait(ctx, PostExitTail)
	stopTraffic()
	traffic.Wait()

	r.postExitCheck(ctx)
	return result, nil
}

// watchExit waits for the process to go, escalating to SIGKILL at grace expiry.
func (r *Runner) watchExit(ctx context.Context, signalAt, grace time.Duration) Result {
	deadline := signalAt + grace
	var result Result

	for {
		alive, err := r.opts.Target.Alive()
		if err == nil && !alive {
			result.ExitStatus = r.recordExit(ctx)
			return result
		}

		if r.now() >= deadline {
			break
		}
		if !r.wait(ctx, ExitPollInterval) {
			return result
		}
	}

	if !r.opts.EnforceSigkill {
		r.notice("warn", "grace_exceeded",
			"the target was still running at grace expiry; a real orchestrator would have killed it here")
		return result
	}

	r.stage(timeline.StageSigkill)
	killAt := r.now()
	killErr := r.opts.Target.Signal(target.SIGKILL)
	r.record(timeline.SignalAt(killAt, timeline.SignalEvent{
		Signal: string(target.SIGKILL),
		Error:  errText(killErr),
	}))
	result.Killed = true

	result.ExitStatus = r.recordExit(ctx)
	return result
}

func (r *Runner) recordExit(ctx context.Context) target.ExitStatus {
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	status, err := r.opts.Target.Wait(waitCtx)
	if err != nil {
		r.notice("warn", "exit_unobserved", err.Error())
		return status
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
	return status
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
			r.record(p.Poll(ctx))
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
