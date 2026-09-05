package load

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shutdowncheck/shutdowncheck/internal/clock"
	"github.com/shutdowncheck/shutdowncheck/internal/probe"
	"github.com/shutdowncheck/shutdowncheck/internal/testutil"
	"github.com/shutdowncheck/shutdowncheck/internal/timeline"
)

func constantLatencies(n int, d time.Duration) []time.Duration {
	out := make([]time.Duration, n)
	for i := range out {
		out[i] = d
	}
	return out
}

func TestCalibrateDerivesRateFromLittlesLaw(t *testing.T) {
	// 20 in flight against a 50ms endpoint needs 400 rps.
	got, err := Calibrate(Input{
		Latencies:      constantLatencies(50, 50*time.Millisecond),
		Total:          50,
		TargetInFlight: 20,
		MaxRPS:         2000,
		ConcurrencyCap: 512,
	})
	if err != nil {
		t.Fatalf("Calibrate: %v", err)
	}

	if got.RPS != 400 {
		t.Errorf("RPS = %v, want 400", got.RPS)
	}
	if got.BaselineLatency != 50*time.Millisecond {
		t.Errorf("baseline = %v, want 50ms", got.BaselineLatency)
	}
	if !got.Achievable {
		t.Error("the goal is comfortably reachable and should be marked achievable")
	}
}

// A 1ms endpoint would need 20,000 rps to hold 20 requests in flight. Rather
// than quietly running at the ceiling and reporting a pass, calibration must
// declare the goal unreachable.
func TestCalibrateFlagsUnreachableGoal(t *testing.T) {
	got, err := Calibrate(Input{
		Latencies:      constantLatencies(50, time.Millisecond),
		Total:          50,
		TargetInFlight: 20,
		MaxRPS:         2000,
		ConcurrencyCap: 512,
	})
	if err != nil {
		t.Fatalf("Calibrate: %v", err)
	}

	if got.Achievable {
		t.Fatal("goal should be unreachable within the rps ceiling")
	}
	if got.RPS != 2000 {
		t.Errorf("RPS = %v, want the ceiling of 2000", got.RPS)
	}
	if len(got.Warnings) == 0 {
		t.Error("an unreachable goal must explain itself")
	}
}

func TestCalibrateRespectsConcurrencyCap(t *testing.T) {
	got, err := Calibrate(Input{
		Latencies:      constantLatencies(50, 100*time.Millisecond),
		Total:          50,
		TargetInFlight: 100,
		MaxRPS:         100000,
		ConcurrencyCap: 10,
	})
	if err != nil {
		t.Fatalf("Calibrate: %v", err)
	}

	if got.Achievable {
		t.Error("a concurrency cap below the goal must mark the goal unreachable")
	}
	if got.ExpectedInFlight > 10.0001 {
		t.Errorf("expected in-flight %v exceeds the cap of 10", got.ExpectedInFlight)
	}
}

func TestCalibrateNeverRoundsPastFractionalCeilings(t *testing.T) {
	for name, input := range map[string]Input{
		"max rps": {
			Latencies: constantLatencies(20, time.Second), Total: 20,
			TargetInFlight: 2, MaxRPS: 1.5, ConcurrencyCap: 10,
		},
		"concurrency": {
			Latencies: constantLatencies(20, 400*time.Millisecond), Total: 20,
			TargetInFlight: 10, MaxRPS: 100, ConcurrencyCap: 3,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Calibrate(input)
			if err != nil {
				t.Fatalf("Calibrate: %v", err)
			}
			if input.MaxRPS > 0 && got.RPS > input.MaxRPS {
				t.Errorf("RPS = %v, exceeds max %v", got.RPS, input.MaxRPS)
			}
			if got.ExpectedInFlight > float64(input.ConcurrencyCap)+1e-9 {
				t.Errorf("expected in flight = %v, exceeds concurrency cap %d", got.ExpectedInFlight, input.ConcurrencyCap)
			}
		})
	}
}

func TestCalibrateRejectsUnhealthyTarget(t *testing.T) {
	_, err := Calibrate(Input{
		Latencies:      constantLatencies(90, 10*time.Millisecond),
		Errors:         10,
		Total:          100,
		TargetInFlight: 20,
		MaxRPS:         2000,
	})

	var unhealthy *ErrTargetUnhealthy
	if !errors.As(err, &unhealthy) {
		t.Fatalf("Calibrate error = %v, want ErrTargetUnhealthy", err)
	}
}

func TestAssessBaselineRejectsUnhealthyFixedRateTarget(t *testing.T) {
	_, _, err := AssessBaseline(constantLatencies(90, 10*time.Millisecond), 10, 100)
	var unhealthy *ErrTargetUnhealthy
	if !errors.As(err, &unhealthy) {
		t.Fatalf("AssessBaseline error = %v, want ErrTargetUnhealthy", err)
	}
}

func TestCalibrateRejectsNoSamples(t *testing.T) {
	if _, err := Calibrate(Input{TargetInFlight: 20, MaxRPS: 2000}); !errors.Is(err, ErrNoSamples) {
		t.Fatalf("Calibrate error = %v, want ErrNoSamples", err)
	}
}

func TestCalibrateRejectsBadGoal(t *testing.T) {
	if _, err := Calibrate(Input{TargetInFlight: 0}); err == nil {
		t.Fatal("a zero in-flight goal must be rejected")
	}
}

// A single cold-start outlier must not drag the baseline, which is why the
// median is used and the earliest samples are discarded.
func TestCalibrateIgnoresColdStartOutliers(t *testing.T) {
	latencies := append(
		[]time.Duration{5 * time.Second, 4 * time.Second},
		constantLatencies(18, 10*time.Millisecond)...,
	)

	got, err := Calibrate(Input{
		Latencies:      latencies,
		Total:          len(latencies),
		TargetInFlight: 10,
		MaxRPS:         100000,
		ConcurrencyCap: 10000,
	})
	if err != nil {
		t.Fatalf("Calibrate: %v", err)
	}

	if got.BaselineLatency != 10*time.Millisecond {
		t.Fatalf("baseline = %v, want 10ms; cold-start samples were not discarded", got.BaselineLatency)
	}
}

func TestCalibrateWarnsOnThinSample(t *testing.T) {
	got, err := Calibrate(Input{
		Latencies:      constantLatencies(2, 10*time.Millisecond),
		Total:          2,
		TargetInFlight: 5,
		MaxRPS:         100000,
		ConcurrencyCap: 10000,
	})
	if err != nil {
		t.Fatalf("Calibrate: %v", err)
	}
	if len(got.Warnings) == 0 {
		t.Error("calibrating from a thin sample should warn")
	}
}

func TestMedian(t *testing.T) {
	odd := median([]time.Duration{3, 1, 2})
	if odd != 2 {
		t.Errorf("median(odd) = %v, want 2", odd)
	}
	even := median([]time.Duration{4, 1, 2, 3})
	if even != 2 {
		t.Errorf("median(even) = %v, want 2 (mean of 2 and 3 truncates)", even)
	}
}

func TestScheduleSpacing(t *testing.T) {
	s := BuildSchedule(100, time.Second)

	if s.Len() != 100 {
		t.Fatalf("Len() = %d, want 100", s.Len())
	}
	if got := s.At(0); got != 0 {
		t.Errorf("At(0) = %v, want 0", got)
	}
	if got := s.At(50); got != 500*time.Millisecond {
		t.Errorf("At(50) = %v, want 500ms", got)
	}
}

func TestScheduleEmptyForNonPositiveInputs(t *testing.T) {
	for _, s := range []Schedule{
		BuildSchedule(0, time.Second),
		BuildSchedule(100, 0),
		BuildSchedule(-5, time.Second),
		BuildSchedule(math.NaN(), time.Second),
		BuildSchedule(math.Inf(1), time.Second),
	} {
		if s.Len() != 0 {
			t.Errorf("expected an empty schedule, got %d dispatches", s.Len())
		}
	}
}

// Weighted selection must hold the mix steady inside every window. With only a
// handful of requests in flight at the signal, a random draw could put anywhere
// from none to many slow requests in flight, and that variance would land
// straight on the verdict.
func TestScheduleWeightsAreExactAndInterleaved(t *testing.T) {
	picker := newPicker([]int{80, 20})

	counts := map[int]int{}
	early := map[int]int{}
	for i := range 100 {
		selected := picker.next()
		counts[selected]++
		if i < 20 {
			early[selected]++
		}
	}
	if counts[0] != 80 || counts[1] != 20 {
		t.Fatalf("mix = %v, want 80/20 exactly", counts)
	}

	// Check the proportion also holds in the first fifth of the window.
	if early[1] != 4 {
		t.Errorf("first 20 dispatches contained %d weighted requests, want 4", early[1])
	}
}

func TestScheduleIsDeterministic(t *testing.T) {
	first := make([]int, 50)
	picker := newPicker([]int{3, 1, 1})
	for i := range first {
		first[i] = picker.next()
	}
	for range 10 {
		next := newPicker([]int{3, 1, 1})
		for i, want := range first {
			if got := next.next(); got != want {
				t.Fatalf("schedule differs at dispatch %d; runs would not be comparable", i)
			}
		}
	}
}

func TestScheduleHandlesZeroWeights(t *testing.T) {
	picker := newPicker([]int{0, 0})
	counts := map[int]int{}
	for range 10 {
		counts[picker.next()]++
	}
	if counts[0] == 0 || counts[1] == 0 {
		t.Errorf("all-zero weights should fall back to equal shares, got %v", counts)
	}
}

func TestScheduleDoesNotMaterializeHugePlan(t *testing.T) {
	s := BuildSchedule(1e12, time.Hour)
	if s.Len() <= 0 {
		t.Fatal("a finite positive schedule should retain its dispatch count")
	}
}

func TestPhaseResultsBoundLatencySamples(t *testing.T) {
	var results phaseResults
	total := maxLatencySamples + 3
	for i := range total {
		results.record(i, time.Duration(i), 0, true, false)
	}

	got := results.finish(total)
	if got.Dispatched != total || got.Completed != total {
		t.Fatalf("counts = %d/%d, want %d/%d", got.Dispatched, got.Completed, total, total)
	}
	if len(got.Latencies) != maxLatencySamples || got.DroppedLatencySamples != 3 {
		t.Fatalf("samples = %d + %d dropped, want %d + 3", len(got.Latencies), got.DroppedLatencySamples, maxLatencySamples)
	}
	if got.Latencies[0] != 0 || got.Latencies[len(got.Latencies)-1] != time.Duration(maxLatencySamples-1) {
		t.Fatal("retained latency samples are not in dispatch order")
	}
}

// fakeProber returns a fixed outcome after a controllable delay.
type fakeProber struct {
	clock       clock.Clock
	latency     time.Duration
	calls       atomic.Int64
	outcome     timeline.Outcome
	inFlight    atomic.Int64
	maxInFlight atomic.Int64
}

func (f *fakeProber) Do(ctx context.Context, _ probe.Request) probe.Attempt {
	f.calls.Add(1)

	n := f.inFlight.Add(1)
	for {
		peak := f.maxInFlight.Load()
		if n <= peak || f.maxInFlight.CompareAndSwap(peak, n) {
			break
		}
	}
	defer f.inFlight.Add(-1)

	if f.latency > 0 {
		select {
		case <-f.clock.After(f.latency):
		case <-ctx.Done():
		}
	}

	outcome := f.outcome
	if outcome == "" {
		outcome = timeline.OutcomeOK
	}
	return probe.Attempt{Status: 200, Outcome: outcome}
}

func newTestGenerator(t *testing.T, p Prober, c clock.Clock, cap int) (*Generator, *timeline.Recorder) {
	t.Helper()

	rec := timeline.NewRecorder(timeline.Meta{}, 0)
	g := New(Options{
		Prober:         p,
		Clock:          c,
		Recorder:       rec,
		Requests:       []probe.Request{{Name: "r", Method: "GET", URL: "http://localhost:8080/"}},
		Weights:        []int{1},
		ConcurrencyCap: cap,
	})
	return g, rec
}

func TestGeneratorDispatchesAtTheScheduledRate(t *testing.T) {
	testutil.NoLeaks(t)

	fake := clock.NewFake(time.Time{})
	origin := fake.Now()
	prober := &fakeProber{clock: fake}
	g, rec := newTestGenerator(t, prober, fake, 100)

	done := make(chan Result, 1)
	go func() {
		done <- g.Run(context.Background(), origin, Phase{Rate: 10, Duration: time.Second})
	}()

	// Drive the fake clock forward until the phase completes.
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case result := <-done:
			if result.Dispatched != 10 {
				t.Fatalf("dispatched %d requests, want 10", result.Dispatched)
			}
			if got := len(rec.Snapshot().Requests()); got != 10 {
				t.Fatalf("recorded %d request events, want 10", got)
			}
			return
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("phase did not complete")
		}
		fake.Advance(10 * time.Millisecond)
		time.Sleep(time.Millisecond)
	}
}

func TestGeneratorPinsSlowRequestsAtPhaseStart(t *testing.T) {
	testutil.NoLeaks(t)

	prober := &fakeProber{clock: clock.System()}
	g, rec := newTestGenerator(t, prober, clock.System(), 10)
	slow := probe.Request{Name: "slow", Method: "GET", URL: "http://localhost:8080/slow"}

	result := g.Run(context.Background(), time.Now(), Phase{
		PinnedRequest: &slow,
		PinnedCount:   3,
	})

	if result.Dispatched != 3 {
		t.Fatalf("dispatched %d requests, want 3 pinned requests", result.Dispatched)
	}
	requests := rec.Snapshot().Requests()
	if len(requests) != 3 {
		t.Fatalf("recorded %d requests, want 3", len(requests))
	}
	for _, request := range requests {
		if request.Definition != "slow" || request.URL != slow.URL {
			t.Errorf("pinned request = %+v", request)
		}
	}
}

type blockingProber struct {
	started chan struct{}
	release chan struct{}
}

func (p *blockingProber) Do(ctx context.Context, _ probe.Request) probe.Attempt {
	p.started <- struct{}{}
	select {
	case <-p.release:
		return probe.Attempt{Status: 200, Outcome: timeline.OutcomeOK}
	case <-ctx.Done():
		return probe.Attempt{Outcome: timeline.OutcomeAbandoned}
	}
}

func TestGeneratorPinsSlowRequestsAtConfiguredOffset(t *testing.T) {
	prober := &blockingProber{started: make(chan struct{}, 2), release: make(chan struct{})}
	ready := make(chan int, 1)
	g, _ := newTestGenerator(t, prober, clock.System(), 4)

	done := make(chan Result, 1)
	start := time.Now()
	go func() {
		done <- g.Run(context.Background(), start, Phase{
			Duration:      150 * time.Millisecond,
			PinnedRequest: &probe.Request{Name: "slow", URL: "http://localhost/slow"},
			PinnedCount:   2,
			PinnedAt:      60 * time.Millisecond,
			PinnedReady:   ready,
		})
	}()

	select {
	case <-prober.started:
		if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
			t.Fatalf("pinned request started too early at %s", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("pinned request never started")
	}
	<-prober.started
	if got := <-ready; got != 2 {
		t.Errorf("PinnedReady = %d, want 2", got)
	}
	close(prober.release)
	<-done
}

// The defining property of an open model: when the server slows down, the
// generator keeps offering the same rate instead of throttling itself to match.
// A closed loop would silently reduce load and hide the degradation.
func TestGeneratorMaintainsOfferedRateWhenServerSlows(t *testing.T) {
	testutil.NoLeaks(t)

	const rate = 200.0
	const duration = 300 * time.Millisecond

	slow := &realSleepProber{latency: 100 * time.Millisecond}
	g, rec := newTestGenerator(t, slow, clock.System(), 1000)

	origin := time.Now()
	result := g.Run(context.Background(), origin, Phase{Rate: rate, Duration: duration})

	wantDispatched := int(rate * duration.Seconds())
	if result.Dispatched < wantDispatched {
		t.Fatalf("dispatched %d requests, want %d; offered load collapsed under a slow server",
			result.Dispatched, wantDispatched)
	}

	requests := rec.Snapshot().Requests()
	if len(requests) != wantDispatched {
		t.Fatalf("recorded %d requests, want %d", len(requests), wantDispatched)
	}

	// Each request must still take roughly the server's latency; if the
	// generator had serialised them, latency would have ballooned.
	for _, r := range requests {
		if r.Latency() > 2*time.Second {
			t.Fatalf("request latency %v suggests the generator serialised work", r.Latency())
		}
	}
}

// When the concurrency cap binds, the wait must be visible as queue delay
// rather than absorbed into the reported service latency.
func TestGeneratorRecordsQueueDelayUnderBackPressure(t *testing.T) {
	testutil.NoLeaks(t)

	slow := &realSleepProber{latency: 60 * time.Millisecond}
	g, rec := newTestGenerator(t, slow, clock.System(), 2)

	origin := time.Now()
	result := g.Run(context.Background(), origin, Phase{Rate: 200, Duration: 200 * time.Millisecond})

	if result.MaxQueueDelay <= 0 {
		t.Fatal("a binding concurrency cap should have produced a measurable queue delay")
	}

	var withDelay int
	for _, r := range rec.Snapshot().Requests() {
		if r.QueueDelay() > 0 {
			withDelay++
		}
		if r.Sent < r.Scheduled {
			t.Fatal("a request was dispatched before it was scheduled")
		}
	}
	if withDelay == 0 {
		t.Error("no request recorded a queue delay")
	}
}

func TestGeneratorHonoursConcurrencyCap(t *testing.T) {
	testutil.NoLeaks(t)

	prober := &realSleepProber{latency: 30 * time.Millisecond}
	g, _ := newTestGenerator(t, prober, clock.System(), 3)

	g.Run(context.Background(), time.Now(), Phase{Rate: 500, Duration: 150 * time.Millisecond})

	if got := prober.maxInFlight.Load(); got > 3 {
		t.Fatalf("peak in-flight was %d, above the cap of 3", got)
	}
}

func TestGeneratorCountsErrors(t *testing.T) {
	testutil.NoLeaks(t)

	prober := &realSleepProber{outcome: timeline.OutcomeRefused}
	g, _ := newTestGenerator(t, prober, clock.System(), 10)

	result := g.Run(context.Background(), time.Now(), Phase{Rate: 100, Duration: 100 * time.Millisecond})

	if result.Errors != result.Completed {
		t.Errorf("errors = %d, completed = %d; every request failed and should be counted",
			result.Errors, result.Completed)
	}
	if len(result.Latencies) != 0 {
		t.Errorf("failed requests must not contribute latency samples, got %d", len(result.Latencies))
	}
}

func TestGeneratorStopsOnContextCancel(t *testing.T) {
	testutil.NoLeaks(t)

	prober := &realSleepProber{latency: 5 * time.Millisecond}
	g, _ := newTestGenerator(t, prober, clock.System(), 10)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	result := g.Run(ctx, time.Now(), Phase{Rate: 100, Duration: 10 * time.Second})

	if result.Dispatched >= 1000 {
		t.Fatalf("dispatched %d requests; cancellation did not stop the phase", result.Dispatched)
	}
}

func TestGeneratorWithoutRequestsIsANoOp(t *testing.T) {
	g := New(Options{Prober: &realSleepProber{}, Clock: clock.System()})

	if got := g.Run(context.Background(), time.Now(), Phase{Rate: 100, Duration: time.Second}); got.Dispatched != 0 {
		t.Fatalf("dispatched %d requests with no definitions", got.Dispatched)
	}
}

func TestGeneratorMarksWarmupRequests(t *testing.T) {
	testutil.NoLeaks(t)

	g, rec := newTestGenerator(t, &realSleepProber{}, clock.System(), 10)
	g.Run(context.Background(), time.Now(), Phase{Rate: 50, Duration: 100 * time.Millisecond, Warmup: true})

	requests := rec.Snapshot().Requests()
	if len(requests) == 0 {
		t.Fatal("no requests were recorded")
	}
	for _, r := range requests {
		if !r.Warmup {
			t.Fatal("warmup-phase requests must be marked so they can be excluded from scoring")
		}
	}
}

func TestGeneratorRedactsRecordedURLs(t *testing.T) {
	testutil.NoLeaks(t)

	rec := timeline.NewRecorder(timeline.Meta{}, 0)
	g := New(Options{
		Prober:         &realSleepProber{},
		Clock:          clock.System(),
		Recorder:       rec,
		Requests:       []probe.Request{{Name: "r", Method: "GET", URL: "http://localhost:8080/x?token=secret"}},
		Weights:        []int{1},
		ConcurrencyCap: 4,
	})

	g.Run(context.Background(), time.Now(), Phase{Rate: 20, Duration: 100 * time.Millisecond})

	for _, r := range rec.Snapshot().Requests() {
		if got := r.URL; got == "" || contains(got, "secret") {
			t.Fatalf("recorded URL leaked a secret: %q", got)
		}
	}
}

func TestGeneratorRedactsRecordedRequestNames(t *testing.T) {
	const secret = "request-name-secret"
	rec := timeline.NewRecorder(timeline.Meta{}, 10)
	g := New(Options{
		Prober:         &realSleepProber{},
		Clock:          clock.System(),
		Recorder:       rec,
		Requests:       []probe.Request{{Name: "token=" + secret, Method: "GET", URL: "http://localhost/"}},
		Weights:        []int{1},
		ConcurrencyCap: 1,
	})

	g.Run(context.Background(), time.Now(), Phase{Rate: 10, Duration: 100 * time.Millisecond})
	requests := rec.Snapshot().Requests()
	if len(requests) != 1 {
		t.Fatalf("recorded %d requests, want 1", len(requests))
	}
	if strings.Contains(requests[0].Definition, secret) {
		t.Fatalf("request definition leaked a secret: %q", requests[0].Definition)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// realSleepProber uses wall-clock sleeps, for tests that exercise genuine
// concurrency rather than deterministic scheduling.
type realSleepProber struct {
	latency     time.Duration
	outcome     timeline.Outcome
	inFlight    atomic.Int64
	maxInFlight atomic.Int64
}

func (p *realSleepProber) Do(ctx context.Context, _ probe.Request) probe.Attempt {
	n := p.inFlight.Add(1)
	for {
		peak := p.maxInFlight.Load()
		if n <= peak || p.maxInFlight.CompareAndSwap(peak, n) {
			break
		}
	}
	defer p.inFlight.Add(-1)

	if p.latency > 0 {
		timer := time.NewTimer(p.latency)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
		}
	}

	outcome := p.outcome
	if outcome == "" {
		outcome = timeline.OutcomeOK
	}
	return probe.Attempt{Status: 200, Outcome: outcome}
}

// Calibration is only useful if the rate it derives actually produces the
// requested concurrency, so the two halves are checked together end to end.
//
// The band is deliberately generous. The exact arithmetic is asserted purely in
// TestCalibrateDerivesRateFromLittlesLaw; what this test guards against is an
// order-of-magnitude error, such as mixing up seconds and milliseconds or using
// a mean instead of a median. A tight band here would only measure how loaded
// the host happens to be, and a gate that flakes is a gate that gets deleted.
//
// The assertion is on mean in-flight, not peak: Little's Law predicts the mean,
// and the instantaneous peak runs higher because timer granularity bunches
// dispatches together.
func TestCalibratedRateProducesTargetInFlight(t *testing.T) {
	testutil.NoLeaks(t)

	const latency = 40 * time.Millisecond
	const target = 8
	const duration = time.Second

	calibration, err := Calibrate(Input{
		Latencies:      constantLatencies(30, latency),
		Total:          30,
		TargetInFlight: target,
		MaxRPS:         5000,
		ConcurrencyCap: 512,
	})
	if err != nil {
		t.Fatalf("Calibrate: %v", err)
	}

	prober := &realSleepProber{latency: latency}
	g, rec := newTestGenerator(t, prober, clock.System(), 512)

	start := time.Now()
	g.Run(context.Background(), start, Phase{Rate: calibration.RPS, Duration: duration})
	elapsed := time.Since(start)

	// Mean concurrency is total busy time divided by wall time: L = lambda * W.
	var busy time.Duration
	requests := rec.Snapshot().Requests()
	for _, r := range requests {
		busy += r.Latency()
	}
	if len(requests) == 0 {
		t.Fatal("no requests were recorded")
	}

	mean := busy.Seconds() / elapsed.Seconds()
	if mean < target/2.0 || mean > target*2.0 {
		t.Fatalf("mean in-flight was %.2f, want within 2x of %d (calibrated to %v rps over %v, %d requests)",
			mean, target, calibration.RPS, elapsed, len(requests))
	}
}
