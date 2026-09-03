// Package clock abstracts time so that schedulers, probes and monitors can be
// tested without sleeping.
//
// It is deliberately separate from internal/timeline. The timeline is a pure
// data model that the analysis engine depends on, so it must not be able to
// reach a clock even indirectly; keeping the two apart makes that structural
// rather than a matter of discipline. See docs/adr/0011-pure-analysis-core.md.
package clock

import (
	"sort"
	"sync"
	"time"
)

// Clock is the subset of the time package that shutdowncheck actually needs.
type Clock interface {
	Now() time.Time
	Since(t time.Time) time.Duration
	Sleep(d time.Duration)
	After(d time.Duration) <-chan time.Time
	NewTicker(d time.Duration) Ticker
}

// Ticker mirrors time.Ticker, but as an interface so it can be faked.
type Ticker interface {
	C() <-chan time.Time
	Stop()
}

// System returns a Clock backed by the real time package.
func System() Clock { return systemClock{} }

type systemClock struct{}

func (systemClock) Now() time.Time                         { return time.Now() }
func (systemClock) Since(t time.Time) time.Duration        { return time.Since(t) }
func (systemClock) Sleep(d time.Duration)                  { time.Sleep(d) }
func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

func (systemClock) NewTicker(d time.Duration) Ticker {
	return &systemTicker{t: time.NewTicker(d)}
}

type systemTicker struct{ t *time.Ticker }

func (s *systemTicker) C() <-chan time.Time { return s.t.C }
func (s *systemTicker) Stop()               { s.t.Stop() }

// Fake is a manually-advanced Clock. Tests drive it with Advance, so a run that
// spans a 30 second grace period completes instantly and deterministically.
//
// Fake is safe for concurrent use.
type Fake struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*waiter
	blocked chan struct{}
}

type waiter struct {
	deadline time.Time
	period   time.Duration // zero for one-shot
	ch       chan time.Time
	stopped  bool
}

// NewFake returns a Fake clock started at the given instant. A zero start is
// replaced with a fixed, arbitrary date so that formatted output in tests is
// stable and obviously synthetic.
func NewFake(start time.Time) *Fake {
	if start.IsZero() {
		start = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	}
	return &Fake{now: start}
}

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *Fake) Since(t time.Time) time.Duration { return f.Now().Sub(t) }

func (f *Fake) Sleep(d time.Duration) { <-f.After(d) }

func (f *Fake) After(d time.Duration) <-chan time.Time {
	return f.addWaiter(d, 0).ch
}

func (f *Fake) NewTicker(d time.Duration) Ticker {
	if d <= 0 {
		panic("clock: NewTicker requires a positive duration")
	}
	return &fakeTicker{fake: f, w: f.addWaiter(d, d)}
}

func (f *Fake) addWaiter(d time.Duration, period time.Duration) *waiter {
	f.mu.Lock()
	defer f.mu.Unlock()

	// Buffered so that firing never blocks Advance, even if nobody is reading.
	w := &waiter{deadline: f.now.Add(d), period: period, ch: make(chan time.Time, 1)}
	f.waiters = append(f.waiters, w)
	f.signalBlockedLocked()
	return w
}

// Advance moves the clock forward, firing every waiter whose deadline is
// reached. Periodic waiters are rescheduled, and fire once per elapsed period.
func (f *Fake) Advance(d time.Duration) {
	if d < 0 {
		panic("clock: cannot advance backwards")
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	target := f.now.Add(d)
	for {
		next, idx := f.earliestLocked(target)
		if idx < 0 {
			break
		}

		f.now = next
		w := f.waiters[idx]
		select {
		case w.ch <- f.now:
		default: // a pending tick is already queued; drop this one, as time.Ticker does
		}

		if w.period > 0 {
			w.deadline = w.deadline.Add(w.period)
		} else {
			f.removeWaiterLocked(idx)
		}
	}
	f.now = target
}

// earliestLocked finds the next waiter due at or before limit.
func (f *Fake) earliestLocked(limit time.Time) (time.Time, int) {
	best := -1
	var bestAt time.Time
	for i, w := range f.waiters {
		if w.stopped || w.deadline.After(limit) {
			continue
		}
		if best < 0 || w.deadline.Before(bestAt) {
			best, bestAt = i, w.deadline
		}
	}
	return bestAt, best
}

func (f *Fake) removeWaiterLocked(i int) {
	f.waiters = append(f.waiters[:i], f.waiters[i+1:]...)
}

// Waiters reports how many timers and tickers are currently pending.
func (f *Fake) Waiters() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	n := 0
	for _, w := range f.waiters {
		if !w.stopped {
			n++
		}
	}
	return n
}

// BlockUntil waits until n waiters are pending. Without it, a test that calls
// Advance before the code under test has registered its timer races and hangs.
func (f *Fake) BlockUntil(n int) {
	for {
		f.mu.Lock()
		count := 0
		for _, w := range f.waiters {
			if !w.stopped {
				count++
			}
		}
		if count >= n {
			f.mu.Unlock()
			return
		}
		if f.blocked == nil {
			f.blocked = make(chan struct{})
		}
		ch := f.blocked
		f.mu.Unlock()
		<-ch
	}
}

func (f *Fake) signalBlockedLocked() {
	if f.blocked != nil {
		close(f.blocked)
		f.blocked = nil
	}
}

// Deadlines returns the pending deadlines in order, for assertions in tests.
func (f *Fake) Deadlines() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]time.Time, 0, len(f.waiters))
	for _, w := range f.waiters {
		if !w.stopped {
			out = append(out, w.deadline)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

type fakeTicker struct {
	fake *Fake
	w    *waiter
}

func (t *fakeTicker) C() <-chan time.Time { return t.w.ch }

func (t *fakeTicker) Stop() {
	t.fake.mu.Lock()
	defer t.fake.mu.Unlock()

	t.w.stopped = true
	for i, w := range t.fake.waiters {
		if w == t.w {
			t.fake.removeWaiterLocked(i)
			return
		}
	}
}
