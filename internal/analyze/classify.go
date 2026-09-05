package analyze

import (
	"sort"
	"time"

	"github.com/shutdowncheck/shutdowncheck/internal/timeline"
)

// Phase is where a request sits relative to the termination signal.
type Phase string

// PhaseWarmup and the other phase constants identify request timing relative
// to the termination signal.
const (
	PhaseWarmup     Phase = "warmup"
	PhaseSteady     Phase = "steady"
	PhaseInFlight   Phase = "in_flight"
	PhasePostSignal Phase = "post_signal"
	PhasePostWindow Phase = "post_window"
)

// Phases lists the buckets in report order.
func Phases() []Phase {
	return []Phase{PhaseWarmup, PhaseSteady, PhaseInFlight, PhasePostSignal, PhasePostWindow}
}

// ClassifiedRequest is a recorded request placed in its shutdown phase.
type ClassifiedRequest struct {
	timeline.RequestEvent
	Phase Phase
	// AtSigkill marks a request that was still open when SIGKILL landed. It is
	// a flag rather than a phase because such a request also belongs to whatever
	// bucket it started in, and losing that would hide where it came from.
	AtSigkill bool
}

// PhaseStats counts outcomes within one phase.
type PhaseStats struct {
	Count    int
	OK       int
	Failed   int
	Failures map[string]int
}

// ConnectionStats summarises connection-layer behaviour.
type ConnectionStats struct {
	Opened                    int
	ReusedAfterSignal         int
	ClosedWithConnectionClose int
	ClosedByFIN               int
	ClosedByRST               int
	ResetAfterSignal          int
	ClosedAfterSignal         int
}

// Percentiles summarises a latency sample.
type Percentiles struct {
	Count         int
	P50, P95, P99 time.Duration
}

// Facts is everything derived from a timeline that signatures need.
//
// Deriving it once keeps each rule short and free of scanning logic, and means
// a signature can be tested by handing it a Facts value directly.
type Facts struct {
	HasSignal  bool
	SignalAt   time.Duration
	SignalSkew time.Duration
	KillAt     *time.Duration

	HasExit    bool
	ExitAt     *time.Duration
	ExitCode   *int
	ExitSignal string

	HadListenerProbe          bool
	ListenerAcceptingAtSignal bool
	ListenerClosedAt          *time.Duration
	AcceptedAfterWindow       bool
	AcceptedAfterExit         bool

	HadReadinessProbe        bool
	ReadinessEverHealthy     bool
	ReadinessHealthyAtSignal bool
	ReadinessFlippedAt       *time.Duration
	ReadinessFlapped         bool

	Requests  []ClassifiedRequest
	Stats     map[Phase]PhaseStats
	AtSigkill int

	Connections ConnectionStats

	BaselineLatency Percentiles
	DrainLatency    Percentiles

	ShutdownDuration      *time.Duration
	DroppedRecords        int
	DroppedRequestRecords int
}

// BuildFacts derives the analysable view of a run.
func BuildFacts(tl timeline.Timeline, policy Policy) Facts {
	facts := Facts{
		Stats:                 map[Phase]PhaseStats{},
		DroppedRecords:        tl.Dropped + tl.DroppedAuxiliary,
		DroppedRequestRecords: tl.Dropped,
	}

	if signal, at, ok := tl.TerminationSignal(); ok {
		facts.HasSignal = true
		facts.SignalAt = at
		facts.SignalSkew = signal.Skew
	}
	if at, ok := tl.KillOffset(); ok {
		kill := at
		facts.KillAt = &kill
	}
	if event, at, ok := tl.ProcessExit(); ok {
		exit := at
		facts.HasExit = true
		facts.ExitAt = &exit
		facts.ExitCode = event.ExitCode
		facts.ExitSignal = event.TerminatedBy

		if facts.HasSignal && exit >= facts.SignalAt {
			duration := exit - facts.SignalAt
			facts.ShutdownDuration = &duration
		}
	}

	facts.classifyRequests(tl, policy)
	facts.summariseListener(tl)
	facts.markAcceptanceAfterWindow(tl, policy)
	facts.summariseReadiness(tl)
	facts.summariseConnections(tl)
	facts.summariseLatency()

	return facts
}

func (f *Facts) classifyRequests(tl timeline.Timeline, policy Policy) {
	windowEnd := f.SignalAt + policy.AcceptWindow

	for _, req := range tl.Requests() {
		classified := ClassifiedRequest{RequestEvent: req, Phase: f.phaseOf(req, windowEnd)}
		if f.KillAt != nil && req.Sent < *f.KillAt && req.Done > *f.KillAt {
			classified.AtSigkill = true
			f.AtSigkill++
		}

		f.Requests = append(f.Requests, classified)
		f.record(classified)
	}
}

// phaseOf places one request.
//
// The boundaries are strict on purpose. A request whose send instant equals the
// signal instant was not yet being processed when the signal arrived, so it is
// post-signal traffic rather than in-flight work; and one that completed exactly
// at the signal had already finished. Treating either as in-flight would inflate
// the bucket the entire verdict turns on.
func (f *Facts) phaseOf(req timeline.RequestEvent, windowEnd time.Duration) Phase {
	switch {
	case req.Warmup:
		return PhaseWarmup
	case !f.HasSignal:
		return PhaseSteady
	case req.Done <= f.SignalAt:
		return PhaseSteady
	case req.Sent < f.SignalAt:
		return PhaseInFlight
	case req.Sent <= windowEnd:
		return PhasePostSignal
	default:
		return PhasePostWindow
	}
}

func (f *Facts) record(req ClassifiedRequest) {
	stats := f.Stats[req.Phase]
	stats.Count++

	if req.Outcome.Succeeded() {
		stats.OK++
	} else {
		stats.Failed++
		if stats.Failures == nil {
			stats.Failures = map[string]int{}
		}
		stats.Failures[string(req.Outcome)]++
	}
	f.Stats[req.Phase] = stats
}

func (f *Facts) summariseListener(tl timeline.Timeline) {
	samples := tl.EventsOfKind(timeline.KindListener)
	if len(samples) == 0 {
		return
	}
	f.HadListenerProbe = true

	for _, e := range samples {
		accepting := e.Listener.Accepting
		if !f.HasSignal || e.Offset < f.SignalAt {
			f.ListenerAcceptingAtSignal = accepting
		}

		if f.HasSignal && e.Offset >= f.SignalAt {
			if !accepting && f.ListenerClosedAt == nil {
				at := e.Offset
				f.ListenerClosedAt = &at
			}
			// A later acceptance means the listener reopened, or never really
			// closed; either way the earlier observation was not the close.
			if accepting && f.ListenerClosedAt != nil {
				f.ListenerClosedAt = nil
			}
		}

		if accepting && f.ExitAt != nil && e.Offset > *f.ExitAt {
			f.AcceptedAfterExit = true
		}
	}
}

// acceptedAfterWindow is computed separately because it depends on the policy's
// accept window rather than on the timeline alone.
func (f *Facts) markAcceptanceAfterWindow(tl timeline.Timeline, policy Policy) {
	if !f.HasSignal {
		return
	}
	windowEnd := f.SignalAt + policy.AcceptWindow

	for _, e := range tl.EventsOfKind(timeline.KindListener) {
		if e.Offset > windowEnd && e.Listener.Accepting {
			f.AcceptedAfterWindow = true
			return
		}
	}
}

func (f *Facts) summariseReadiness(tl timeline.Timeline) {
	samples := tl.EventsOfKind(timeline.KindReadiness)
	if len(samples) == 0 {
		return
	}
	f.HadReadinessProbe = true

	for _, e := range samples {
		healthy := e.Readiness.Healthy

		if !f.HasSignal || e.Offset < f.SignalAt {
			f.ReadinessHealthyAtSignal = healthy
			if healthy {
				f.ReadinessEverHealthy = true
			}
			continue
		}

		switch {
		case !healthy && f.ReadinessFlippedAt == nil:
			at := e.Offset
			f.ReadinessFlippedAt = &at
		case healthy && f.ReadinessFlippedAt != nil:
			// Recovering after starting to fail re-registers a terminating
			// instance with the load balancer.
			f.ReadinessFlapped = true
		}
	}
}

func (f *Facts) summariseConnections(tl timeline.Timeline) {
	for _, e := range tl.EventsOfKind(timeline.KindConnection) {
		conn := e.Connection
		afterSignal := f.HasSignal && e.Offset >= f.SignalAt

		switch conn.Phase {
		case timeline.ConnOpen:
			f.Connections.Opened++
		case timeline.ConnReuse:
			if afterSignal {
				f.Connections.ReusedAfterSignal++
			}
		case timeline.ConnClose:
			if afterSignal {
				f.Connections.ClosedAfterSignal++
			}
			if conn.ServerClose {
				f.Connections.ClosedWithConnectionClose++
			}
			switch conn.Termination {
			case timeline.TermFIN:
				f.Connections.ClosedByFIN++
			case timeline.TermRST:
				f.Connections.ClosedByRST++
				if afterSignal {
					f.Connections.ResetAfterSignal++
				}
			}
		}
	}
}

func (f *Facts) summariseLatency() {
	var baseline, drain []time.Duration

	for _, req := range f.Requests {
		if !req.Outcome.Succeeded() {
			continue
		}
		switch {
		case req.Phase == PhaseSteady:
			baseline = append(baseline, req.Latency())
		case f.HasSignal && req.Done > f.SignalAt && req.Phase != PhaseWarmup:
			drain = append(drain, req.Latency())
		}
	}

	f.BaselineLatency = percentilesOf(baseline)
	f.DrainLatency = percentilesOf(drain)
}

// percentilesOf uses nearest-rank so that a percentile is always an observed
// value, which keeps reports reproducible and explainable.
func percentilesOf(samples []time.Duration) Percentiles {
	if len(samples) == 0 {
		return Percentiles{}
	}

	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	return Percentiles{
		Count: len(sorted),
		P50:   nearestRank(sorted, 0.50),
		P95:   nearestRank(sorted, 0.95),
		P99:   nearestRank(sorted, 0.99),
	}
}

func nearestRank(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted))*q+0.9999) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// Stat returns the statistics for a phase, including zero values for phases
// with no traffic so that callers never have to check for absence.
func (f Facts) Stat(p Phase) PhaseStats { return f.Stats[p] }

// InFlightDropRatio is the fraction of in-flight requests that failed.
func (f Facts) InFlightDropRatio() float64 {
	stats := f.Stats[PhaseInFlight]
	if stats.Count == 0 {
		return 0
	}
	return float64(stats.Failed) / float64(stats.Count)
}
