package load

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

// Calibration guard rails, per spec section 7.5.
const (
	// WarmupDiscardFraction drops the earliest samples, which are dominated by
	// cold caches, lazy connection setup and JIT rather than steady-state
	// behaviour.
	WarmupDiscardFraction = 0.2
	// MaxBaselineErrorRate aborts calibration against a service that is already
	// failing: any verdict drawn from it would describe the pre-existing fault,
	// not the shutdown.
	MaxBaselineErrorRate = 0.01
	// MinCalibrationSamples is the fewest successful samples worth a median.
	MinCalibrationSamples = 5
)

// ErrNoSamples reports that warmup produced nothing to calibrate from.
var ErrNoSamples = errors.New("no successful warmup samples; the target did not serve any requests")

// ErrTargetUnhealthy reports a target that was already failing before the
// experiment began.
type ErrTargetUnhealthy struct {
	Errors, Total int
	Rate          float64
}

func (e *ErrTargetUnhealthy) Error() string {
	return fmt.Sprintf(
		"target failed %d of %d warmup requests (%.1f%%), above the %.0f%% ceiling; "+
			"it is already unhealthy, so a shutdown verdict would be meaningless",
		e.Errors, e.Total, e.Rate*100, MaxBaselineErrorRate*100)
}

// Input is everything calibration needs. Latencies must be in send order so
// that the warmup discard removes the earliest samples.
type Input struct {
	Latencies      []time.Duration
	Errors         int
	Total          int
	TargetInFlight int
	MaxRPS         float64
	ConcurrencyCap int
}

// Calibration is the derived load plan.
type Calibration struct {
	RPS              float64
	BaselineLatency  time.Duration
	ExpectedInFlight float64
	// Achievable is false when the in-flight goal cannot be reached within the
	// configured ceilings. The run may still proceed, but its verdict is capped
	// at inconclusive.
	Achievable bool
	Warnings   []string
}

// Calibrate derives the request rate needed to hold TargetInFlight requests in
// flight when the signal lands.
//
// By Little's Law L = lambda * W, so lambda = L / W where W is the service's own
// median response time. Deriving the rate rather than defaulting it is what
// stops the tool reporting a pass on a fast endpoint it never actually caught
// mid-request; see docs/adr/0004-auto-calibrated-load.md.
func Calibrate(in Input) (Calibration, error) {
	if in.TargetInFlight < 1 {
		return Calibration{}, fmt.Errorf("target in-flight must be at least 1, got %d", in.TargetInFlight)
	}

	baseline, warnings, err := AssessBaseline(in.Latencies, in.Errors, in.Total)
	if err != nil {
		return Calibration{}, err
	}
	out := Calibration{Achievable: true, BaselineLatency: baseline, Warnings: warnings}

	seconds := baseline.Seconds()
	ideal := float64(in.TargetInFlight) / seconds

	rate := math.Ceil(ideal)
	if in.MaxRPS > 0 && rate > in.MaxRPS {
		rate = in.MaxRPS
		out.Achievable = false
		out.Warnings = append(out.Warnings, fmt.Sprintf(
			"holding %d requests in flight against a %s endpoint needs %.0f rps, above the %.0f rps ceiling; "+
				"point --slow-url at a slower endpoint or raise --max-rps",
			in.TargetInFlight, baseline, math.Ceil(ideal), in.MaxRPS))
	}

	// The concurrency cap bounds sockets, and therefore bounds in-flight work
	// regardless of the arrival rate.
	if in.ConcurrencyCap > 0 {
		capRate := float64(in.ConcurrencyCap) / seconds
		if rate > capRate {
			rate = capRate
			out.Achievable = false
			out.Warnings = append(out.Warnings, fmt.Sprintf(
				"concurrency cap of %d limits in-flight requests below the goal of %d; raise --concurrency-cap",
				in.ConcurrencyCap, in.TargetInFlight))
		}
	}

	out.RPS = rate
	out.ExpectedInFlight = out.RPS * seconds
	if out.ExpectedInFlight+1e-9 < float64(in.TargetInFlight) {
		out.Achievable = false
	}
	return out, nil
}

// AssessBaseline validates warmup health and returns its steady-state median.
// Fixed-rate runs use the same precondition checks as calibrated runs even
// though they do not derive a new rate.
func AssessBaseline(latencies []time.Duration, errorCount, total int) (time.Duration, []string, error) {
	if errorCount < 0 || total < 0 || errorCount > total {
		return 0, nil, fmt.Errorf("invalid warmup counts: %d errors from %d requests", errorCount, total)
	}
	if total > 0 {
		if rate := float64(errorCount) / float64(total); rate > MaxBaselineErrorRate {
			return 0, nil, &ErrTargetUnhealthy{Errors: errorCount, Total: total, Rate: rate}
		}
	}

	samples := steadySamples(latencies)
	if len(samples) == 0 {
		return 0, nil, ErrNoSamples
	}

	var warnings []string
	if len(samples) < MinCalibrationSamples {
		warnings = append(warnings, fmt.Sprintf(
			"baseline uses only %d samples; increase --warmup for a more reliable measurement", len(samples)))
	}
	baseline := median(samples)
	if baseline <= 0 {
		return 0, nil, ErrNoSamples
	}
	return baseline, warnings, nil
}

// steadySamples drops the leading warmup fraction, always keeping at least one
// sample.
func steadySamples(latencies []time.Duration) []time.Duration {
	if len(latencies) == 0 {
		return nil
	}

	drop := int(float64(len(latencies)) * WarmupDiscardFraction)
	if drop >= len(latencies) {
		drop = len(latencies) - 1
	}
	return latencies[drop:]
}

// median is used rather than the mean because a single cold-start outlier would
// drag a mean upward and silently halve the derived rate.
func median(samples []time.Duration) time.Duration {
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}
