package timeline

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Record limits bound request evidence. Connection and observer evidence gets
// a larger derived budget because a request can legitimately produce multiple
// connection events. Lifecycle events are always retained.
const (
	DefaultRecordLimit  = 100_000
	MaxRecordLimit      = 250_000
	auxiliaryMultiplier = 2
)

// Recorder is the single sink every observer writes into. It is safe for
// concurrent use and never blocks a caller, because a probe stalling on the
// recorder would distort the very latencies it is trying to measure.
type Recorder struct {
	mu               sync.Mutex
	meta             Meta
	events           []Event
	seq              uint64
	limit            int
	requests         int
	auxiliary        int
	dropped          int
	droppedAuxiliary int
	overflowed       bool
}

// NewRecorder returns a Recorder. Non-positive limits use the safe default.
func NewRecorder(meta Meta, limit int) *Recorder {
	if limit <= 0 {
		limit = DefaultRecordLimit
	}
	if limit > MaxRecordLimit {
		limit = MaxRecordLimit
	}
	return &Recorder{meta: meta, limit: limit}
}

// Record stores an observation, assigning it a sequence number.
//
// Request records use the configured limit. Connection and observer records
// use a derived budget large enough for open/close events around every request.
// Stage, signal, process, log and notice events do not consume either budget.
// Logs are independently byte- and line-bounded by target.lineWriter.
func (r *Recorder) Record(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()

	overflow := e.Kind == KindRequest && r.requests >= r.limit
	if auxiliaryEvent(e.Kind) && r.auxiliary >= auxiliaryMultiplier*r.limit {
		overflow = true
	}
	if overflow {
		if e.Kind == KindRequest {
			r.dropped++
		} else {
			r.droppedAuxiliary++
		}
		if !r.overflowed {
			r.overflowed = true
			r.appendLocked(NoticeAt(e.Offset, "warn", "record_limit_reached", fmt.Sprintf(
				"record limit of %d reached; further high-volume evidence is not retained and this run is inconclusive",
				r.limit,
			)))
		}
		return
	}

	r.appendLocked(e)
	if e.Kind == KindRequest {
		r.requests++
	} else if auxiliaryEvent(e.Kind) {
		r.auxiliary++
	}
}

func auxiliaryEvent(kind Kind) bool {
	switch kind {
	case KindConnection, KindReadiness, KindListener:
		return true
	default:
		return false
	}
}

func (r *Recorder) appendLocked(e Event) {
	r.seq++
	e.Seq = r.seq
	r.events = append(r.events, e)
}

// RecordAll stores several observations atomically, keeping their relative
// order.
func (r *Recorder) RecordAll(events ...Event) {
	for _, e := range events {
		r.Record(e)
	}
}

// Len reports how many events are currently stored.
func (r *Recorder) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

// Dropped reports how many high-volume evidence records were discarded.
func (r *Recorder) Dropped() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropped + r.droppedAuxiliary
}

// Snapshot returns an immutable copy of everything recorded so far, ordered by
// offset and then sequence.
//
// Producers run concurrently, so arrival order is not observation order.
// Sorting here gives analysis one canonical ordering, which is what makes
// repeated analysis of the same run byte-for-byte reproducible.
func (r *Recorder) Snapshot() Timeline {
	r.mu.Lock()
	defer r.mu.Unlock()

	events := make([]Event, len(r.events))
	copy(events, r.events)
	sortEvents(events)

	return Timeline{
		Meta: r.meta, Events: events,
		Dropped: r.dropped, DroppedAuxiliary: r.droppedAuxiliary,
	}
}

func sortEvents(events []Event) {
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].Offset != events[j].Offset {
			return events[i].Offset < events[j].Offset
		}
		return events[i].Seq < events[j].Seq
	})
}

// Timeline is an immutable record of a run. Analysis consumes it and nothing
// else; see docs/adr/0011-pure-analysis-core.md.
type Timeline struct {
	Meta             Meta
	Events           []Event
	Dropped          int
	DroppedAuxiliary int
}

// EventsOfKind returns every event of the given kind, in timeline order.
func (t Timeline) EventsOfKind(k Kind) []Event {
	var out []Event
	for _, e := range t.Events {
		if e.Kind == k {
			out = append(out, e)
		}
	}
	return out
}

// First returns the earliest event of the given kind.
func (t Timeline) First(k Kind) (Event, bool) {
	for _, e := range t.Events {
		if e.Kind == k {
			return e, true
		}
	}
	return Event{}, false
}

// Last returns the latest event of the given kind.
func (t Timeline) Last(k Kind) (Event, bool) {
	for i := len(t.Events) - 1; i >= 0; i-- {
		if t.Events[i].Kind == k {
			return t.Events[i], true
		}
	}
	return Event{}, false
}

// Requests returns every recorded request, in completion order.
func (t Timeline) Requests() []RequestEvent {
	var out []RequestEvent
	for _, e := range t.Events {
		if e.Kind == KindRequest && e.Request != nil {
			out = append(out, *e.Request)
		}
	}
	return out
}

// SignalOffset returns when the terminating signal was delivered.
//
// Only the first non-KILL signal counts: the SIGKILL escalation at grace expiry
// is a separate event, and treating it as the origin would collapse the whole
// drain window.
func (t Timeline) SignalOffset() (time.Duration, bool) {
	_, offset, ok := t.TerminationSignal()
	return offset, ok
}

// TerminationSignal returns the first graceful signal and its offset.
func (t Timeline) TerminationSignal() (SignalEvent, time.Duration, bool) {
	for _, e := range t.Events {
		if e.Kind == KindSignal && e.Signal != nil && e.Signal.Signal != "KILL" {
			return *e.Signal, e.Offset, true
		}
	}
	return SignalEvent{}, 0, false
}

// KillOffset returns when SIGKILL was delivered, if it was.
func (t Timeline) KillOffset() (time.Duration, bool) {
	for _, e := range t.Events {
		if e.Kind == KindSignal && e.Signal != nil && e.Signal.Signal == "KILL" {
			return e.Offset, true
		}
	}
	return 0, false
}

// ProcessExit returns the process-exit event, if the process exited.
func (t Timeline) ProcessExit() (ProcessEvent, time.Duration, bool) {
	for _, e := range t.Events {
		if e.Kind == KindProcess && e.Process != nil && e.Process.Phase == ProcExited {
			return *e.Process, e.Offset, true
		}
	}
	return ProcessEvent{}, 0, false
}
