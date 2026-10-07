package analyze

import (
	"fmt"
	"time"

	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/timeline"
)

// registry holds every detection rule. Each is independent: it reads Facts and
// decides, without knowing what any other rule concluded.
var registry = []Signature{
	ruleInsufficientInFlight,
	ruleSigtermIgnored,
	ruleSigkillRequired,
	ruleInFlightDropped,
	ruleAbruptConnectionReset,
	ruleListenerOpenAfterWindow,
	ruleNoDeregistrationWindow,
	ruleReadinessNotFlipped,
	ruleReadinessFlipSlow,
	ruleKeepaliveNotTerminated,
	ruleShutdownBudgetExceeded,
	ruleEarlyExit,
	rulePortHeldAfterExit,
	ruleNonZeroExitCode,
	ruleDrainLatencySpike,
	ruleAcceptWithoutResponse,
	ruleReadinessFlapped,
	rulePostKillTrafficLoss,
}

// SC000: the experiment did not produce enough in-flight requests to conclude
// anything. This is what stops a fast endpoint yielding a meaningless pass.
var ruleInsufficientInFlight = rule{
	id: SC000,
	eval: func(f Facts, p Policy) (string, map[string]any, bool) {
		if !f.HasSignal {
			return "No termination signal was recorded, so nothing about shutdown could be measured.",
				map[string]any{"signal_recorded": false}, true
		}
		if f.DroppedRecords > 0 {
			return fmt.Sprintf(
					"%d evidence record(s) were omitted after the recording limit was reached, so shutdown cannot be judged from incomplete evidence.",
					f.DroppedRecords),
				map[string]any{"dropped_records": f.DroppedRecords}, true
		}
		if f.SignalSkew > MaxSignalSkew {
			return fmt.Sprintf(
					"The termination signal landed %s late, beyond the %s timing budget, so request phases cannot be trusted.",
					f.SignalSkew, MaxSignalSkew),
				map[string]any{"signal_skew_ms": f.SignalSkew.Milliseconds(), "budget_ms": MaxSignalSkew.Milliseconds()}, true
		}
		if f.HadReadinessProbe && !f.ReadinessHealthyAtSignal {
			return "The configured readiness endpoint was not healthy when the signal landed, so shutdown cannot be separated from a pre-existing health failure.",
				map[string]any{"readiness_healthy_at_signal": false}, true
		}
		if f.HadListenerProbe && !f.ListenerAcceptingAtSignal {
			return "The target listener was not accepting connections when the signal landed, so shutdown cannot be separated from a pre-existing availability failure.",
				map[string]any{"listener_accepting_at_signal": false}, true
		}
		steady := f.Stat(PhaseSteady)
		if steady.Count > 0 && float64(steady.Failed)/float64(steady.Count) > 0.01 {
			return fmt.Sprintf(
					"The target failed %d of %d steady-state requests before the signal, so shutdown cannot be separated from a pre-existing load failure.",
					steady.Failed, steady.Count),
				map[string]any{"steady_failures": steady.Failed, "steady_requests": steady.Count}, true
		}

		got := f.Stat(PhaseInFlight).Count
		if got >= p.MinInFlightSample {
			return "", nil, false
		}
		return fmt.Sprintf(
				"Only %d request(s) were in flight when the signal landed; %d are needed before draining can be judged.",
				got, p.MinInFlightSample),
			map[string]any{"in_flight": got, "required": p.MinInFlightSample}, true
	},
}

// SC001: nothing observable changed for the whole grace budget.
var ruleSigtermIgnored = rule{
	id: SC001,
	eval: func(f Facts, p Policy) (string, map[string]any, bool) {
		if !f.HasSignal {
			return "", nil, false
		}

		// Each clause is only evidence if the corresponding probe actually ran;
		// an unconfigured probe must never be mistaken for "no reaction".
		listenerUnchanged := !f.HadListenerProbe || f.ListenerClosedAt == nil
		readinessUnchanged := !f.HadReadinessProbe || f.ReadinessFlippedAt == nil
		stillRunning := f.ExitAt == nil || *f.ExitAt > f.SignalAt+p.GracePeriod

		if !listenerUnchanged || !readinessUnchanged || !stillRunning {
			return "", nil, false
		}
		return "The process showed no reaction to the signal: it kept listening, kept reporting itself healthy, and did not exit.",
			map[string]any{
				"listener_closed":     f.HadListenerProbe && f.ListenerClosedAt != nil,
				"readiness_flipped":   f.HadReadinessProbe && f.ReadinessFlippedAt != nil,
				"exited_within_grace": !stillRunning,
			}, true
	},
}

// SC002: the orchestrator would have had to hard-kill this process.
var ruleSigkillRequired = rule{
	id: SC002,
	eval: func(f Facts, p Policy) (string, map[string]any, bool) {
		if !f.HasSignal {
			return "", nil, false
		}

		evidence := map[string]any{"grace_period_ms": p.GracePeriod.Milliseconds()}

		switch {
		case f.KillAt != nil:
			evidence["killed_at_ms"] = f.KillAt.Milliseconds()
			return fmt.Sprintf("The process was still running %s after the signal and had to be killed.",
				p.GracePeriod), evidence, true

		case f.ExitAt == nil:
			return "The process never exited.", evidence, true

		case f.ShutdownDuration != nil && *f.ShutdownDuration > p.GracePeriod:
			evidence["shutdown_ms"] = f.ShutdownDuration.Milliseconds()
			return fmt.Sprintf("Shutdown took %s, beyond the %s grace period.",
				*f.ShutdownDuration, p.GracePeriod), evidence, true
		}
		return "", nil, false
	},
}

// SC003: the headline defect. Work already in progress was destroyed.
var ruleInFlightDropped = rule{
	id: SC003,
	eval: func(f Facts, _ Policy) (string, map[string]any, bool) {
		stats := f.Stat(PhaseInFlight)
		if stats.Failed == 0 {
			return "", nil, false
		}

		evidence := map[string]any{"in_flight_total": stats.Count, "in_flight_failed": stats.Failed}
		for outcome, count := range stats.Failures {
			evidence["failed_"+outcome] = count
		}
		return fmt.Sprintf("%d of %d in-flight request(s) were destroyed during shutdown.",
			stats.Failed, stats.Count), evidence, true
	},
}

// resetTeardownAllowance is the number of post-signal connection resets a
// run may show before SC004 treats them as a defect. When a process or
// container exits, requests already sitting in kernel socket buffers are
// answered with RST by the kernel itself; no server, however correct, can
// close those connections cleanly, so a handful of resets is what ordinary
// teardown looks like under load. Resets beyond the allowance mean the
// server is destroying sockets systematically rather than occasionally
// losing the race against its own exit.
const resetTeardownAllowance = 5

// SC004: sockets destroyed rather than closed.
var ruleAbruptConnectionReset = rule{
	id: SC004,
	eval: func(f Facts, _ Policy) (string, map[string]any, bool) {
		// For a Docker target the client connections terminate at Docker's
		// proxy, which holds them open while the service drains and resets
		// all of them when the container dies, however cleanly the service
		// itself closed its side. The reset count then measures the proxy,
		// not the service, so it is reported in the connections section but
		// never judged. The harm a reset causes is still judged, through the
		// requests it destroys (SC003).
		if f.TargetKind == TargetDocker {
			return "", nil, false
		}
		if f.Connections.ResetAfterSignal <= resetTeardownAllowance {
			return "", nil, false
		}
		return fmt.Sprintf("%d connection(s) were reset rather than closed cleanly after the signal.",
				f.Connections.ResetAfterSignal),
			map[string]any{
				"reset_after_signal": f.Connections.ResetAfterSignal,
				"closed_by_fin":      f.Connections.ClosedByFIN,
				"teardown_allowance": resetTeardownAllowance,
			}, true
	},
}

// dockerAcceptanceSlack is how long a Docker target's published port may
// keep accepting past the accept window before SC005 judges it. The port
// belongs to Docker's proxy, so after the signal the delivery through the
// daemon, the service's own close, and the proxy noticing the container is
// gone each take real time a process target does not spend; acceptance
// ending within the slack is that machinery winding down. Acceptance
// beyond it means the service was still listening, which the run's own
// evidence shows directly: requests kept being answered.
const dockerAcceptanceSlack = 2 * time.Second

// SC005: still pulling in traffic it cannot finish.
var ruleListenerOpenAfterWindow = rule{
	id: SC005,
	eval: func(f Facts, p Policy) (string, map[string]any, bool) {
		if !f.HasSignal {
			return "", nil, false
		}
		// For a Docker target the probed listener is the proxy's, and the
		// probe cannot tell the service's listener from the proxy's while
		// the container lives. What can be judged is when acceptance
		// actually ended, so the docker path uses the observed close time
		// with the slack above instead of the raw probe samples.
		if f.TargetKind == TargetDocker {
			if f.ListenerClosedAt != nil &&
				*f.ListenerClosedAt-f.SignalAt <= p.AcceptWindow+dockerAcceptanceSlack {
				return "", nil, false
			}
			evidence := map[string]any{
				"accept_window_ms":           p.AcceptWindow.Milliseconds(),
				"docker_acceptance_slack_ms": dockerAcceptanceSlack.Milliseconds(),
			}
			if f.ListenerClosedAt != nil {
				evidence["closed_after_ms"] = (*f.ListenerClosedAt - f.SignalAt).Milliseconds()
			}
			return fmt.Sprintf("The listener was still accepting connections more than %s after the signal.",
					p.AcceptWindow),
				evidence, true
		}
		if !f.AcceptedAfterWindow {
			return "", nil, false
		}
		return fmt.Sprintf("The listener was still accepting connections more than %s after the signal.",
				p.AcceptWindow),
			map[string]any{"accept_window_ms": p.AcceptWindow.Milliseconds()}, true
	},
}

// SC006: closed the door before the load balancer stopped sending people to it.
// This is the subtle, widespread failure the tool exists to surface.
var ruleNoDeregistrationWindow = rule{
	id: SC006,
	eval: func(f Facts, p Policy) (string, map[string]any, bool) {
		if !f.HasSignal || f.ListenerClosedAt == nil {
			return "", nil, false
		}

		gap := *f.ListenerClosedAt - f.SignalAt
		if gap >= p.DeregMin {
			return "", nil, false
		}
		return fmt.Sprintf(
				"The listener closed %s after the signal, before de-registration could propagate.", gap),
			map[string]any{
				"closed_after_ms": gap.Milliseconds(),
				"minimum_ms":      p.DeregMin.Milliseconds(),
			}, true
	},
}

// SC007: the load balancer was never told to stop sending traffic.
var ruleReadinessNotFlipped = rule{
	id: SC007,
	eval: func(f Facts, _ Policy) (string, map[string]any, bool) {
		if !f.HasSignal || !f.HadReadinessProbe || !f.ReadinessEverHealthy {
			return "", nil, false
		}
		if f.ReadinessFlippedAt != nil {
			return "", nil, false
		}
		return "The readiness endpoint kept reporting healthy for the entire shutdown.",
			map[string]any{"readiness_flipped": false}, true
	},
}

// SC008: told, but too late.
var ruleReadinessFlipSlow = rule{
	id: SC008,
	eval: func(f Facts, p Policy) (string, map[string]any, bool) {
		if f.ReadinessFlippedAt == nil {
			return "", nil, false
		}

		delay := *f.ReadinessFlippedAt - f.SignalAt
		if delay <= p.ReadinessFlipBudget {
			return "", nil, false
		}
		return fmt.Sprintf("Readiness took %s to start failing, beyond the %s budget.",
				delay, p.ReadinessFlipBudget),
			map[string]any{
				"flipped_after_ms": delay.Milliseconds(),
				"budget_ms":        p.ReadinessFlipBudget.Milliseconds(),
			}, true
	},
}

// SC009: clients left holding a socket that is about to die.
var ruleKeepaliveNotTerminated = rule{
	id: SC009,
	eval: func(f Facts, _ Policy) (string, map[string]any, bool) {
		if f.Connections.ReusedAfterSignal == 0 || f.Connections.ClosedWithConnectionClose > 0 {
			return "", nil, false
		}
		return fmt.Sprintf(
				"%d connection(s) were reused after the signal, but the server never asked clients to close them.",
				f.Connections.ReusedAfterSignal),
			map[string]any{
				"reused_after_signal":   f.Connections.ReusedAfterSignal,
				"connection_close_sent": f.Connections.ClosedWithConnectionClose,
			}, true
	},
}

// SC010: inside the orchestrator's grace, but outside the budget the team
// declared for itself.
var ruleShutdownBudgetExceeded = rule{
	id: SC010,
	eval: func(f Facts, p Policy) (string, map[string]any, bool) {
		if p.MaxShutdownTime == nil || f.ShutdownDuration == nil {
			return "", nil, false
		}
		if *f.ShutdownDuration <= *p.MaxShutdownTime {
			return "", nil, false
		}
		return fmt.Sprintf("Shutdown took %s, beyond the declared budget of %s.",
				*f.ShutdownDuration, *p.MaxShutdownTime),
			map[string]any{
				"shutdown_ms": f.ShutdownDuration.Milliseconds(),
				"budget_ms":   p.MaxShutdownTime.Milliseconds(),
			}, true
	},
}

// SC011: walked out while still holding work it had accepted.
var ruleEarlyExit = rule{
	id: SC011,
	eval: func(f Facts, _ Policy) (string, map[string]any, bool) {
		if f.ExitAt == nil {
			return "", nil, false
		}

		abandoned := 0
		for _, req := range f.Requests {
			if req.Phase == PhaseWarmup || req.Outcome.Succeeded() {
				continue
			}
			// A refused request never reached the server: the dial failed, so
			// it wasn't accepted work and can't have been abandoned. Without
			// this, a post-signal dial that completes a hair after the process
			// exit timestamp flaps SC011 on servers that did everything right.
			if req.Outcome == timeline.OutcomeRefused {
				continue
			}
			if req.Sent < *f.ExitAt && req.Done > *f.ExitAt {
				abandoned++
			}
		}
		if abandoned == 0 {
			return "", nil, false
		}
		return fmt.Sprintf("%d request(s) were still being processed when the process exited.", abandoned),
			map[string]any{"abandoned": abandoned}, true
	},
}

// SC012: an orphaned child kept the port.
var rulePortHeldAfterExit = rule{
	id: SC012,
	eval: func(f Facts, _ Policy) (string, map[string]any, bool) {
		if !f.AcceptedAfterExit {
			return "", nil, false
		}
		return "The port was still accepting connections after the main process exited, so something it spawned outlived it.",
			map[string]any{"port_held_after_exit": true}, true
	},
}

// SC013: only fires on an exit code we actually observed. An attached process's
// code is unavailable, and inventing a zero there would be a false pass.
var ruleNonZeroExitCode = rule{
	id: SC013,
	eval: func(f Facts, _ Policy) (string, map[string]any, bool) {
		if f.ExitCode == nil || *f.ExitCode == 0 {
			return "", nil, false
		}
		return fmt.Sprintf("The process exited with status %d after being asked to stop.", *f.ExitCode),
			map[string]any{"exit_code": *f.ExitCode}, true
	},
}

// SC014: technically served, but slowly enough that callers time out.
var ruleDrainLatencySpike = rule{
	id: SC014,
	eval: func(f Facts, p Policy) (string, map[string]any, bool) {
		if f.BaselineLatency.Count == 0 || f.DrainLatency.Count == 0 || f.BaselineLatency.P99 <= 0 {
			return "", nil, false
		}

		factor := float64(f.DrainLatency.P99) / float64(f.BaselineLatency.P99)
		if factor <= p.LatencySpikeFactor {
			return "", nil, false
		}
		return fmt.Sprintf("Latency during drain rose to %.1fx the baseline (p99 %s against %s).",
				factor, f.DrainLatency.P99, f.BaselineLatency.P99),
			map[string]any{
				"factor":          round2(factor),
				"drain_p99_ms":    f.DrainLatency.P99.Milliseconds(),
				"baseline_p99_ms": f.BaselineLatency.P99.Milliseconds(),
			}, true
	},
}

// SC015: the worst shape of failure for a caller, because it hangs instead of
// failing fast.
var ruleAcceptWithoutResponse = rule{
	id: SC015,
	eval: func(f Facts, _ Policy) (string, map[string]any, bool) {
		if !f.HasSignal {
			return "", nil, false
		}

		timeouts := 0
		for _, req := range f.Requests {
			if req.Phase != PhasePostSignal && req.Phase != PhasePostWindow {
				continue
			}
			if req.Outcome == timeline.OutcomeTimeout {
				timeouts++
			}
		}
		if timeouts == 0 {
			return "", nil, false
		}
		return fmt.Sprintf("%d request(s) after the signal were accepted but never answered.", timeouts),
			map[string]any{"timeouts_after_signal": timeouts}, true
	},
}

// SC016: recovered after starting to fail, which re-registers a dying instance.
var ruleReadinessFlapped = rule{
	id: SC016,
	eval: func(f Facts, _ Policy) (string, map[string]any, bool) {
		if !f.ReadinessFlapped {
			return "", nil, false
		}
		return "Readiness started failing and then reported healthy again during shutdown.",
			map[string]any{"readiness_flapped": true}, true
	},
}

// SC017: quantifies what the hard kill actually destroyed.
var rulePostKillTrafficLoss = rule{
	id: SC017,
	eval: func(f Facts, _ Policy) (string, map[string]any, bool) {
		if f.AtSigkill == 0 {
			return "", nil, false
		}
		return fmt.Sprintf("%d request(s) were still in flight when SIGKILL landed.", f.AtSigkill),
			map[string]any{"in_flight_at_sigkill": f.AtSigkill}, true
	},
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}
