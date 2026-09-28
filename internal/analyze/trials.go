package analyze

import (
	"errors"

	"github.com/Ashutosh-Panda2004/ShutdownCheck/pkg/schema"
)

// ErrNoTrials reports an aggregation with nothing to aggregate.
var ErrNoTrials = errors.New("no trial results to aggregate")

// Aggregate combines repeated trials into a single verdict.
//
// A shutdown that fails one run in three is flaky in production too, so an
// inconsistent result is reported rather than averaged away: the run that found
// a defect is the one that carries information.
func Aggregate(results []Result) (Result, error) {
	aggregated, _, err := AggregateWithSource(results)
	return aggregated, err
}

// AggregateWithSource also reports which trial supplied the verdict and
// evidence. Callers that retain per-trial timelines must use the same source,
// or a failing report can be paired with a passing trial's observations.
func AggregateWithSource(results []Result) (Result, int, error) {
	if len(results) == 0 {
		return Result{}, 0, ErrNoTrials
	}

	worst := 0
	failed := 0
	consistent := true

	for i, result := range results {
		if result.Report.Verdict == schema.VerdictFail {
			failed++
		}
		if result.Report.Verdict != results[0].Report.Verdict {
			consistent = false
		}
		if verdictRank(result.Report.Verdict) > verdictRank(results[worst].Report.Verdict) {
			worst = i
		}
	}

	// Among equally-bad verdicts, the lowest score is the most informative.
	for i, result := range results {
		if result.Report.Verdict == results[worst].Report.Verdict &&
			result.Score.Value < results[worst].Score.Value {
			worst = i
		}
	}

	aggregated := results[worst]
	aggregated.Report.Run.Trials = schema.Trials{
		Total:      len(results),
		Failed:     failed,
		Consistent: consistent,
	}
	return aggregated, worst, nil
}

// verdictRank orders verdicts by how much they should dominate an aggregate.
//
// A failure outranks an inconclusive run because it is positive evidence of a
// defect, whereas inconclusive is the absence of evidence. Both outrank a pass,
// since a pass can never override either.
func verdictRank(v schema.Verdict) int {
	switch v {
	case schema.VerdictFail:
		return 2
	case schema.VerdictInconclusive:
		return 1
	default:
		return 0
	}
}
