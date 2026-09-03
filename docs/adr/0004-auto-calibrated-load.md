# ADR-0004: Auto-calibrate load; there is no default requests-per-second

- **Status:** Accepted
- **Date:** 2026-09-03

## Context

To detect a drain bug, requests must actually be in flight at the instant the termination signal lands. Whether that happens depends on the interaction between the request rate and the service's response time — neither of which the user should have to reason about before their first run.

The v0.1 draft asked what the right default `--rps` and `--duration` would be. There is no correct answer: a rate that produces twenty concurrent requests against a 60ms endpoint produces essentially none against a 2ms endpoint.

This matters far more than a usability detail. **A tool that reports `PASS` because it never had any requests in flight is actively harmful** — it manufactures confidence in a service that may drop traffic on every deploy.

## Decision

Do not ship a meaningful default rate. Instead, calibrate to a target in-flight concurrency, defaulting to `--ensure-in-flight 20`.

By Little's Law, `L = λW`, so the required arrival rate is `λ = L_target / W_baseline`, where `W_baseline` is the median response time measured during warmup.

The algorithm, its clamps, and its guard rails are specified in section 7.5 of the specification. The decisive part is the failure behaviour:

- If the in-flight goal cannot be reached within `--max-rps`, emit `SC000 INSUFFICIENT_INFLIGHT` and cap the verdict at `INCONCLUSIVE`.
- If fewer than `--min-inflight-sample` requests (default 5) were genuinely in flight at the signal, the verdict is **`INCONCLUSIVE`, never `PASS`**.
- If the baseline error rate exceeds 1%, abort during preflight: the service is already unhealthy and any result would be meaningless.

An explicit `--rps` overrides calibration for users who know what they want.

## Consequences

- The first run works without tuning, which is the single largest adoption lever the tool has.
- `INCONCLUSIVE` must be a first-class verdict with its own exit code (`2`), so CI can distinguish "this build has a shutdown bug" from "this experiment proved nothing".
- Warmup adds a few seconds to every run. Acceptable against a target total runtime of well under 30 seconds.
- Calibration is a measurement, so it can be wrong. It is therefore verified during steady state, adjusted at most once, and then held fixed — a rate that drifts through the signal would corrupt phase attribution.

## Alternatives considered

- **A fixed default such as `--rps 20`.** Rejected: it silently produces false `PASS` results on fast endpoints, which is the worst possible failure mode for this tool.
- **Closed-loop load (N workers looping).** Rejected: offered load then collapses as the server slows, which is textbook coordinated omission and would hide the very degradation we are measuring. The scheduler is open-model for this reason.
- **Require the user to specify the rate.** Rejected: it pushes a subtle queueing-theory calculation onto every user, and most will get it wrong in the direction of a false `PASS`.
