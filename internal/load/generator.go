package load

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shutdowncheck/shutdowncheck/internal/clock"
	"github.com/shutdowncheck/shutdowncheck/internal/probe"
	"github.com/shutdowncheck/shutdowncheck/internal/timeline"
)

// Prober issues a single request. It is an interface so the generator can be
// tested against a fake clock with no sockets involved.
type Prober interface {
	Do(ctx context.Context, req probe.Request) probe.Attempt
}

// Options configures a generator.
type Options struct {
	Prober         Prober
	Clock          clock.Clock
	Recorder       *timeline.Recorder
	Requests       []probe.Request
	Weights        []int
	ConcurrencyCap int
}

// Generator produces rate-controlled traffic.
type Generator struct {
	opts   Options
	nextID atomic.Uint64
}

// New builds a generator.
func New(opts Options) *Generator {
	if opts.Clock == nil {
		opts.Clock = clock.System()
	}
	if opts.ConcurrencyCap <= 0 {
		opts.ConcurrencyCap = 512
	}
	return &Generator{opts: opts}
}

// Phase is one segment of traffic at a fixed rate.
type Phase struct {
	Rate     float64
	Duration time.Duration
	// Warmup marks requests as excluded from scoring while still recording them.
	Warmup bool
}

// Result summarises a phase.
type Result struct {
	Dispatched    int
	Completed     int
	Errors        int
	Latencies     []time.Duration
	MaxQueueDelay time.Duration
}

// Run generates traffic for one phase and returns when every dispatched request
// has completed.
//
// origin is the run's zero point; every recorded offset is relative to it, so
// that traffic, readiness, listener and process events all share one timeline.
func (g *Generator) Run(ctx context.Context, origin time.Time, phase Phase) Result {
	if len(g.opts.Requests) == 0 {
		return Result{}
	}

	schedule := BuildSchedule(phase.Rate, phase.Duration, g.opts.Weights)
	if schedule.Len() == 0 {
		return Result{}
	}

	var (
		phaseStart = g.opts.Clock.Since(origin)
		count      = schedule.Len()
		latencies  = make([]time.Duration, count)
		succeeded  = make([]bool, count)
		queued     = make([]time.Duration, count)
		failed     = make([]bool, count)
		completed  = make([]bool, count)
		sem        = make(chan struct{}, g.opts.ConcurrencyCap)
		wg         sync.WaitGroup
	)

	dispatched := 0
	for i := range count {
		scheduledAt := phaseStart + schedule.At(i)
		if !g.waitUntil(ctx, origin, scheduledAt) {
			break
		}

		// Acquiring the slot is where back-pressure shows up. Blocking here
		// delays this dispatch and every one after it, and that delay is
		// recorded rather than silently absorbed.
		var acquired bool
		select {
		case sem <- struct{}{}:
			acquired = true
		case <-ctx.Done():
		}
		if !acquired {
			break
		}
		if ctx.Err() != nil {
			<-sem
			break
		}

		dispatched++
		wg.Add(1)
		go func(index int, scheduled time.Duration) {
			defer wg.Done()
			defer func() { <-sem }()

			latency, queueDelay, ok, isError := g.one(ctx, origin, index, scheduled, schedule, phase)
			latencies[index] = latency
			queued[index] = queueDelay
			succeeded[index] = ok
			failed[index] = isError
			completed[index] = true
		}(i, scheduledAt)
	}
	wg.Wait()

	result := Result{Dispatched: dispatched}
	for i := range count {
		if !completed[i] {
			continue
		}
		result.Completed++
		if queued[i] > result.MaxQueueDelay {
			result.MaxQueueDelay = queued[i]
		}
		if failed[i] {
			result.Errors++
		}
		if succeeded[i] {
			result.Latencies = append(result.Latencies, latencies[i])
		}
	}
	return result
}

func (g *Generator) one(
	ctx context.Context,
	origin time.Time,
	index int,
	scheduled time.Duration,
	schedule Schedule,
	phase Phase,
) (latency, queueDelay time.Duration, ok, isError bool) {
	req := g.opts.Requests[schedule.Request(index)%len(g.opts.Requests)]

	sent := g.opts.Clock.Since(origin)
	attempt := g.opts.Prober.Do(ctx, req)
	done := g.opts.Clock.Since(origin)

	event := timeline.RequestEvent{
		ID:         g.nextID.Add(1),
		Definition: req.Name,
		Method:     req.Method,
		URL:        probe.RedactURL(req.URL),
		Scheduled:  scheduled,
		Sent:       sent,
		Done:       done,
		Status:     attempt.Status,
		Outcome:    attempt.Outcome,
		Error:      attempt.Error,
		ConnID:     attempt.ConnID,
		ConnReused: attempt.ConnReused,
		Warmup:     phase.Warmup,
	}
	if g.opts.Recorder != nil {
		g.opts.Recorder.Record(timeline.RequestAt(event))
	}

	return event.Latency(), event.QueueDelay(), attempt.Outcome.Succeeded(), !attempt.Outcome.Succeeded()
}

// waitUntil blocks until the run has reached the given offset. It reports false
// if the context ended first.
func (g *Generator) waitUntil(ctx context.Context, origin time.Time, offset time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}

	remaining := offset - g.opts.Clock.Since(origin)
	if remaining <= 0 {
		return true
	}

	select {
	case <-g.opts.Clock.After(remaining):
		return true
	case <-ctx.Done():
		return false
	}
}
