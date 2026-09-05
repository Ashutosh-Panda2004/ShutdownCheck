package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/shutdowncheck/shutdowncheck/internal/config"
)

// minimumRunCeiling keeps the derived ceiling sane for very short runs, where
// three times almost nothing is still almost nothing.
const (
	minimumRunCeiling = 2 * time.Minute
	maximumRunCeiling = 30 * 24 * time.Hour
)

// runCeiling returns the wall-clock limit for the whole invocation.
//
// It is derived rather than fixed, because a 5s grace period and a thirty-trial
// run against a 60s grace period are legitimately very different lengths. The
// multiplier is deliberately generous: this is a backstop against a hang, not a
// performance budget, so tripping it should mean something is genuinely stuck
// rather than that the machine was busy.
func runCeiling(r *config.Resolved) time.Duration {
	readiness := r.Target.Ready.Timeout
	if readiness < 30*time.Second {
		readiness = 30 * time.Second
	}
	perTrial := r.Traffic.Warmup +
		r.Traffic.Steady +
		r.Termination.PreStopSleep +
		r.Termination.GracePeriod +
		readiness

	trials := r.Trials
	if trials < 1 {
		trials = 1
	}

	ceiling := time.Duration(trials) * perTrial * 3
	if ceiling < minimumRunCeiling {
		return minimumRunCeiling
	}
	if ceiling > maximumRunCeiling {
		return maximumRunCeiling
	}
	return ceiling
}

// errRunCeiling reports that the whole run exceeded its wall-clock limit.
type errRunCeiling struct{ limit time.Duration }

func (e *errRunCeiling) Error() string {
	return fmt.Sprintf(
		"the run exceeded its %s ceiling and was aborted; "+
			"the target most likely never became ready or never exited. "+
			"Raise it with --timeout, or report a bug if you believe shutdowncheck itself is stuck",
		e.limit)
}

// withCeiling bounds ctx, and reports whether the ceiling was what ended it.
//
// A run that hangs forever is worse than one that fails: it burns a CI runner
// until the job times out, and produces no report either way.
func withCeiling(ctx context.Context, limit time.Duration) (context.Context, context.CancelFunc, func(error) error) {
	bounded, cancel := context.WithTimeout(ctx, limit)

	// The distinction matters: a user interrupting the run is not the same
	// event as the tool giving up on a wedged target.
	classify := func(err error) error {
		if err == nil {
			return nil
		}
		if errors.Is(bounded.Err(), context.DeadlineExceeded) {
			return &targetError{&errRunCeiling{limit: limit}}
		}
		return err
	}
	return bounded, cancel, classify
}

// warnInsecure is printed to stderr rather than folded into the report, because
// the person who needs to see it is the one running the command.
func warnInsecure(w io.Writer, insecure bool) {
	if !insecure {
		return
	}
	writeBestEffort(w, "\nWARNING: --insecure disables TLS certificate verification.\n"+
		"         Traffic to this target is not authenticated, so the verdict\n"+
		"         describes whatever answered, not necessarily your service.\n"+
		"         This is recorded in the report.\n\n")
}
