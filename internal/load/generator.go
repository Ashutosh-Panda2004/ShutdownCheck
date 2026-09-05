package load

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shutdowncheck/shutdowncheck/internal/clock"
	"github.com/shutdowncheck/shutdowncheck/internal/probe"
	"github.com/shutdowncheck/shutdowncheck/internal/redact"
	"github.com/shutdowncheck/shutdowncheck/internal/timeline"
)

// Successful latency samples are useful for calibration but do not need to
// grow without bound during an unusually long phase.
const maxLatencySamples = 100_000

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
	opts.Requests = append([]probe.Request(nil), opts.Requests...)
	for i := range opts.Requests {
		opts.Requests[i].Name = redact.Message(opts.Requests[i].Name)
		opts.Requests[i].URL = redact.URL(opts.Requests[i].URL)
	}
	return &Generator{opts: opts}
}

// Phase is one segment of traffic at a fixed rate.
type Phase struct {
	Rate     float64
	Duration time.Duration
	// Warmup marks requests as excluded from scoring while still recording them.
	Warmup bool
	// PinnedRequest is dispatched PinnedCount times at PinnedAt. PinnedReady
	// receives the number whose HTTP attempts have actually started.
	PinnedRequest *probe.Request
	PinnedCount   int
	PinnedAt      time.Duration
	PinnedReady   chan<- int
}

// Result summarises a phase.
type Result struct {
	Dispatched            int
	Completed             int
	Errors                int
	Latencies             []time.Duration
	DroppedLatencySamples int
	MaxQueueDelay         time.Duration
}

type indexedLatency struct {
	index   int
	latency time.Duration
}

type phaseResults struct {
	mu        sync.Mutex
	result    Result
	latencies []indexedLatency
}

func (r *phaseResults) record(index int, latency, queueDelay time.Duration, succeeded, failed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.result.Completed++
	if queueDelay > r.result.MaxQueueDelay {
		r.result.MaxQueueDelay = queueDelay
	}
	if failed {
		r.result.Errors++
	}
	if succeeded {
		if len(r.latencies) < maxLatencySamples {
			r.latencies = append(r.latencies, indexedLatency{index: index, latency: latency})
		} else {
			r.result.DroppedLatencySamples++
		}
	}
}

func (r *phaseResults) finish(dispatched int) Result {
	sort.Slice(r.latencies, func(i, j int) bool { return r.latencies[i].index < r.latencies[j].index })
	r.result.Dispatched = dispatched
	r.result.Latencies = make([]time.Duration, len(r.latencies))
	for i, sample := range r.latencies {
		r.result.Latencies[i] = sample.latency
	}
	return r.result
}

// Run generates traffic for one phase and returns when every dispatched request
// has completed.
//
// origin is the run's zero point; every recorded offset is relative to it, so
// that traffic, readiness, listener and process events all share one timeline.
func (g *Generator) Run(ctx context.Context, origin time.Time, phase Phase) Result {
	if len(g.opts.Requests) == 0 && phase.PinnedRequest == nil {
		return Result{}
	}

	schedule := BuildSchedule(phase.Rate, phase.Duration)
	pinnedCount := phase.PinnedCount
	if phase.PinnedRequest == nil || pinnedCount < 0 {
		pinnedCount = 0
	}
	if pinnedCount > g.opts.ConcurrencyCap {
		pinnedCount = g.opts.ConcurrencyCap
	}
	if schedule.Len() == 0 && pinnedCount == 0 {
		return Result{}
	}

	var (
		phaseStart = g.opts.Clock.Since(origin)
		sem        = make(chan struct{}, g.opts.ConcurrencyCap)
		wg         sync.WaitGroup
		results    phaseResults
	)

	dispatched := 0
	dispatch := func(index int, scheduled time.Duration, req probe.Request, started chan<- struct{}) bool {
		var acquired bool
		select {
		case sem <- struct{}{}:
			acquired = true
		case <-ctx.Done():
		}
		if !acquired {
			return false
		}
		if ctx.Err() != nil {
			<-sem
			return false
		}

		dispatched++
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			latency, queueDelay, ok, isError := g.one(ctx, origin, req, scheduled, phase, started)
			results.record(index, latency, queueDelay, ok, isError)
		}()
		return true
	}

	pinnedAt := phaseStart + phase.PinnedAt
	pinnedDone := pinnedCount == 0
	dispatchPinned := func() {
		started := make(chan struct{}, pinnedCount)
		launched := 0
		for i := range pinnedCount {
			if !dispatch(i, pinnedAt, *phase.PinnedRequest, started) {
				break
			}
			launched++
		}
		for range launched {
			<-started
		}
		if phase.PinnedReady != nil {
			phase.PinnedReady <- launched
		}
		pinnedDone = true
	}

	picker := newPicker(g.opts.Weights)
	for i := range schedule.Len() {
		scheduledAt := phaseStart + schedule.At(i)
		if !pinnedDone && pinnedAt <= scheduledAt {
			if !g.waitUntil(ctx, origin, pinnedAt) {
				break
			}
			dispatchPinned()
		}
		if !g.waitUntil(ctx, origin, scheduledAt) {
			break
		}
		request := g.opts.Requests[picker.next()%len(g.opts.Requests)]
		index := i + pinnedCount
		if index < i {
			index = int(^uint(0) >> 1)
		}
		if !dispatch(index, scheduledAt, request, nil) {
			break
		}
	}
	if !pinnedDone && g.waitUntil(ctx, origin, pinnedAt) {
		dispatchPinned()
	}
	wg.Wait()
	return results.finish(dispatched)
}

func (g *Generator) one(
	ctx context.Context,
	origin time.Time,
	req probe.Request,
	scheduled time.Duration,
	phase Phase,
	started chan<- struct{},
) (latency, queueDelay time.Duration, ok, isError bool) {
	sent := g.opts.Clock.Since(origin)
	if started != nil {
		started <- struct{}{}
	}
	attempt := g.opts.Prober.Do(ctx, req)
	done := g.opts.Clock.Since(origin)

	event := timeline.RequestEvent{
		ID:         g.nextID.Add(1),
		Definition: req.Name,
		Method:     req.Method,
		URL:        req.URL,
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
