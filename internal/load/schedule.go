package load

import (
	"math"
	"time"
)

// Schedule is a precomputed list of dispatch instants and which request
// definition each one uses.
//
// Computing arrival times up front is what makes the generator open-model: a
// slowing server delays dispatch, and that delay is measured, instead of
// quietly reducing the offered rate. A closed loop of N workers would hide
// exactly the degradation the tool is looking for.
type Schedule struct {
	rate  float64
	count int
	picks []int
}

// BuildSchedule lays out dispatches for a phase.
//
// Selection between weighted request definitions is deterministic rather than
// random. With only ~20 requests in flight, random selection at a 20% weight
// could put anywhere from zero to eight slow requests in flight at the signal,
// and that variance lands directly on the verdict. Smooth weighted round-robin
// holds the mix steady in every window, so repeated runs are comparable.
func BuildSchedule(rate float64, duration time.Duration, weights []int) Schedule {
	if rate <= 0 || duration <= 0 {
		return Schedule{}
	}

	count := int(math.Floor(rate * duration.Seconds()))
	if count < 0 {
		count = 0
	}

	s := Schedule{rate: rate, count: count}
	if count == 0 {
		return s
	}

	s.picks = make([]int, count)
	p := newPicker(weights)
	for i := range s.picks {
		s.picks[i] = p.next()
	}
	return s
}

// Len is the number of dispatches.
func (s Schedule) Len() int { return s.count }

// At returns the offset from phase start at which dispatch i should occur.
func (s Schedule) At(i int) time.Duration {
	if s.rate <= 0 {
		return 0
	}
	return time.Duration(float64(i) / s.rate * float64(time.Second))
}

// Request returns the request-definition index for dispatch i.
func (s Schedule) Request(i int) int {
	if i < 0 || i >= len(s.picks) {
		return 0
	}
	return s.picks[i]
}

// picker implements smooth weighted round-robin: each step adds every weight to
// a running counter, serves the highest, and subtracts the total. The result
// interleaves definitions evenly instead of emitting them in runs.
type picker struct {
	weights []int
	current []int
	total   int
}

func newPicker(weights []int) *picker {
	p := &picker{weights: make([]int, len(weights)), current: make([]int, len(weights))}

	for i, w := range weights {
		if w < 0 {
			w = 0
		}
		p.weights[i] = w
		p.total += w
	}

	// Every weight zero, or no definitions at all: fall back to equal shares so
	// that traffic is still generated.
	if p.total == 0 {
		for i := range p.weights {
			p.weights[i] = 1
		}
		p.total = len(p.weights)
	}
	return p
}

func (p *picker) next() int {
	if len(p.weights) == 0 {
		return 0
	}

	best := 0
	for i := range p.weights {
		p.current[i] += p.weights[i]
		if p.current[i] > p.current[best] {
			best = i
		}
	}
	p.current[best] -= p.total
	return best
}
