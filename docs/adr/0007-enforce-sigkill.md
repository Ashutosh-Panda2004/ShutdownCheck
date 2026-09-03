# ADR-0007: Actually send SIGKILL at grace expiry

- **Status:** Accepted
- **Date:** 2026-09-03

## Context

Real orchestrators escalate. Kubernetes sends `SIGTERM`, waits `terminationGracePeriodSeconds`, then sends `SIGKILL`. `docker stop` does the same with a 10 second default.

A tool could simply observe that the process was still alive at grace expiry and report that the orchestrator *would have* killed it. That is cheaper and gentler, but it stops short of measuring the thing that actually matters: what the hard kill destroys.

## Decision

Deliver a real `SIGKILL` at grace expiry. `--enforce-sigkill` defaults to `true`.

Requests still in flight when the kill lands are recorded in a dedicated `at_sigkill` phase and reported as `SC017 POST_KILL_TRAFFIC_LOSS`, quantifying the damage rather than predicting it.

## Consequences

- The run becomes a rehearsal of the deploy rather than an approximation of it. "37 requests were destroyed by the hard kill" is evidence; "your process would have been killed" is a forecast.
- Runs are bounded. A target that ignores `SIGTERM` cannot hang the tool or, worse, leak a process into a CI runner that then poisons subsequent jobs.
- The tool holds genuinely destructive power, so the safety rules in ADR-0011's sibling constraints apply strictly: never signal PID <= 1, never signal shutdowncheck's own process group, and only ever escalate against a target the tool positively identified.
- Users testing a process they care about could lose it. Mitigated by `--enforce-sigkill=false` and by documenting prominently that the tool terminates what you point it at — but the default stays `true`, because a shutdown verification tool that does not verify the kill path is not doing its job.

## Alternatives considered

- **Observe only, never escalate.** Rejected: it cannot measure post-kill loss, and it allows a hung target to leave the run and the process hanging.
- **Escalate only after a longer safety margin.** Rejected: the point is fidelity to the configured grace period. A different boundary would measure a deployment that does not exist.
