package cli

import (
	"testing"
	"time"

	"github.com/shutdowncheck/shutdowncheck/internal/config"
)

func TestRunCeilingIncludesConfiguredReadinessTimeout(t *testing.T) {
	resolved := &config.Resolved{
		Target: config.Target{Ready: config.Ready{Timeout: 10 * time.Minute}},
		Traffic: config.Traffic{
			Warmup: time.Second,
			Steady: time.Second,
		},
		Termination: config.Termination{GracePeriod: 10 * time.Second},
		Trials:      1,
	}

	if got := runCeiling(resolved); got < 10*time.Minute {
		t.Errorf("ceiling = %s, shorter than readiness timeout", got)
	}
}

func TestRunCeilingKeepsMinimumForShortRuns(t *testing.T) {
	resolved := &config.Resolved{Trials: 1}
	if got := runCeiling(resolved); got != minimumRunCeiling {
		t.Errorf("ceiling = %s, want minimum %s", got, minimumRunCeiling)
	}
}

func TestRunCeilingCapsExtremeMultiTrialRuns(t *testing.T) {
	resolved := &config.Resolved{
		Target:      config.Target{Ready: config.Ready{Timeout: config.MaxOperationalDuration}},
		Traffic:     config.Traffic{Warmup: config.MaxOperationalDuration, Steady: config.MaxOperationalDuration},
		Termination: config.Termination{PreStopSleep: config.MaxOperationalDuration, GracePeriod: config.MaxOperationalDuration},
		Trials:      config.MaxTrials,
	}
	if got := runCeiling(resolved); got != maximumRunCeiling {
		t.Errorf("ceiling = %s, want maximum %s", got, maximumRunCeiling)
	}
}
