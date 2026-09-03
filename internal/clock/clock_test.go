package clock

import (
	"sync"
	"testing"
	"time"
)

func TestSystemClockAdvances(t *testing.T) {
	c := System()

	start := c.Now()
	c.Sleep(2 * time.Millisecond)

	if elapsed := c.Since(start); elapsed <= 0 {
		t.Fatalf("Since() = %v, want a positive duration", elapsed)
	}
}

func TestSystemTickerStops(t *testing.T) {
	c := System()

	tk := c.NewTicker(time.Millisecond)
	defer tk.Stop()

	select {
	case <-tk.C():
	case <-time.After(2 * time.Second):
		t.Fatal("system ticker never fired")
	}
}

func TestFakeStartsAtFixedInstantWhenZero(t *testing.T) {
	f := NewFake(time.Time{})

	want := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	if got := f.Now(); !got.Equal(want) {
		t.Fatalf("Now() = %v, want %v", got, want)
	}
}

func TestFakeAdvanceMovesNow(t *testing.T) {
	start := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)
	f := NewFake(start)

	f.Advance(90 * time.Second)

	if got := f.Since(start); got != 90*time.Second {
		t.Fatalf("Since(start) = %v, want 90s", got)
	}
}

func TestFakeAfterFiresOnlyWhenDeadlineReached(t *testing.T) {
	f := NewFake(time.Time{})
	ch := f.After(5 * time.Second)

	f.Advance(4 * time.Second)
	select {
	case <-ch:
		t.Fatal("timer fired early")
	default:
	}

	f.Advance(time.Second)
	select {
	case at := <-ch:
		if got := at.Sub(f.Now()); got != 0 {
			t.Fatalf("timer fired at %v, want the deadline instant", at)
		}
	default:
		t.Fatal("timer did not fire at its deadline")
	}
}

// A timer must observe the clock as it was at its own deadline, not at the end
// of the Advance call. Analysis correlates events on this timeline, so an
// overshoot here would silently distort every measurement built on it.
func TestFakeTimerSeesDeadlineNotOvershoot(t *testing.T) {
	start := time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC)
	f := NewFake(start)
	ch := f.After(time.Second)

	f.Advance(10 * time.Second)

	at := <-ch
	if want := start.Add(time.Second); !at.Equal(want) {
		t.Fatalf("timer fired reporting %v, want %v", at, want)
	}
	if got := f.Now(); !got.Equal(start.Add(10 * time.Second)) {
		t.Fatalf("Now() = %v, want the full advance to have been applied", got)
	}
}

func TestFakeTimersFireInDeadlineOrder(t *testing.T) {
	f := NewFake(time.Time{})

	third := f.After(30 * time.Second)
	first := f.After(10 * time.Second)
	second := f.After(20 * time.Second)

	f.Advance(time.Minute)

	got := []time.Time{<-first, <-second, <-third}
	for i := 1; i < len(got); i++ {
		if got[i].Before(got[i-1]) {
			t.Fatalf("timers fired out of order: %v", got)
		}
	}
}

func TestFakeTickerIsPeriodic(t *testing.T) {
	f := NewFake(time.Time{})

	tk := f.NewTicker(20 * time.Millisecond)
	defer tk.Stop()

	// Drain each tick so the ticker's single-slot buffer never coalesces.
	ticks := 0
	for range 5 {
		f.Advance(20 * time.Millisecond)
		select {
		case <-tk.C():
			ticks++
		default:
		}
	}

	if ticks != 5 {
		t.Fatalf("got %d ticks, want 5", ticks)
	}
}

// time.Ticker drops ticks rather than queueing them when the receiver is slow.
// The fake must behave the same way, or tests would see a burst that real code
// never observes.
func TestFakeTickerCoalescesMissedTicks(t *testing.T) {
	f := NewFake(time.Time{})

	tk := f.NewTicker(time.Second)
	defer tk.Stop()

	f.Advance(10 * time.Second)

	received := 0
	for {
		select {
		case <-tk.C():
			received++
			continue
		default:
		}
		break
	}

	if received != 1 {
		t.Fatalf("got %d queued ticks, want 1 (missed ticks must be dropped)", received)
	}
}

func TestFakeTickerStopRemovesWaiter(t *testing.T) {
	f := NewFake(time.Time{})

	tk := f.NewTicker(time.Second)
	if got := f.Waiters(); got != 1 {
		t.Fatalf("Waiters() = %d, want 1", got)
	}

	tk.Stop()
	if got := f.Waiters(); got != 0 {
		t.Fatalf("Waiters() after Stop = %d, want 0", got)
	}

	f.Advance(10 * time.Second)
	select {
	case <-tk.C():
		t.Fatal("stopped ticker fired")
	default:
	}
}

func TestFakeOneShotTimerIsRemovedAfterFiring(t *testing.T) {
	f := NewFake(time.Time{})
	f.After(time.Second)

	f.Advance(2 * time.Second)

	if got := f.Waiters(); got != 0 {
		t.Fatalf("Waiters() = %d, want 0 after the one-shot fired", got)
	}
}

func TestFakeSleepBlocksUntilAdvanced(t *testing.T) {
	f := NewFake(time.Time{})

	done := make(chan struct{})
	go func() {
		f.Sleep(time.Second)
		close(done)
	}()

	f.BlockUntil(1)
	select {
	case <-done:
		t.Fatal("Sleep returned before the clock advanced")
	default:
	}

	f.Advance(time.Second)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Sleep did not return after the clock advanced")
	}
}

func TestFakeBlockUntilReturnsImmediatelyWhenSatisfied(t *testing.T) {
	f := NewFake(time.Time{})
	f.After(time.Second)
	f.After(time.Second)

	done := make(chan struct{})
	go func() {
		f.BlockUntil(2)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("BlockUntil hung despite enough waiters already existing")
	}
}

func TestFakeDeadlinesAreSorted(t *testing.T) {
	f := NewFake(time.Time{})
	f.After(30 * time.Second)
	f.After(10 * time.Second)
	f.After(20 * time.Second)

	got := f.Deadlines()
	if len(got) != 3 {
		t.Fatalf("Deadlines() returned %d entries, want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Before(got[i-1]) {
			t.Fatalf("Deadlines() not sorted: %v", got)
		}
	}
}

func TestFakeAdvanceBackwardsPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("advancing backwards should panic")
		}
	}()

	NewFake(time.Time{}).Advance(-time.Second)
}

func TestFakeNewTickerRejectsNonPositive(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewTicker with a non-positive duration should panic")
		}
	}()

	NewFake(time.Time{}).NewTicker(0)
}

func TestFakeIsSafeForConcurrentUse(t *testing.T) {
	f := NewFake(time.Time{})

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				f.After(time.Second)
				_ = f.Now()
				_ = f.Waiters()
				f.Advance(time.Millisecond)
			}
		}()
	}
	wg.Wait()
}
