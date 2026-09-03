# ADR-0005: Shutdown profiles, not a tolerance window

- **Status:** Accepted
- **Date:** 2026-09-03

## Context

The v0.1 draft contained this verdict rule:

> FAIL if any `after-sigterm` request succeeded and was accepted more than a small configurable tolerance after SIGTERM (this catches "still accepting new connections" bugs).

**This rule is wrong for Kubernetes and for most load-balanced deployments**, and the draft's open question — how large the tolerance should be — was the wrong question.

Removing an instance from a load balancer is asynchronous and concurrent with signal delivery. The endpoints controller, kube-proxy, the CNI, the service mesh and any external load balancer all update their routing independently, frequently hundreds of milliseconds to several seconds *after* the process receives `SIGTERM`. A service that closes its listener the instant the signal arrives therefore produces a burst of `connection refused`, which the ingress converts into **502s for real users, on every deploy**.

Under Kubernetes, continuing to accept new connections after `SIGTERM` is not a bug. It is the required behaviour. The bug is failing to *stop* once de-registration has propagated, and separately, failing to flip readiness immediately.

A tolerance window cannot express this, because the same observation — a connection accepted 200ms after the signal — is correct under one deployment model and incorrect under another.

## Decision

Replace the tolerance window with explicit **shutdown profiles**. A profile defines what correct behaviour means for a deployment model:

| Profile | Accept window | New connections in window | After window | Readiness |
|---|---|---|---|---|
| `standalone` | 0s | either | must be refused | warn if absent |
| `strict` | 0s | must be refused immediately | must be refused | warn if absent |
| `lame-duck` | 5s | **must be accepted and succeed** | must be refused | must flip within 1s |
| `kubernetes` | `--prestop-sleep`, else 5s | **must be accepted and succeed** | must be refused | must flip within 1s |
| `docker` | 0s | either | must be refused | warn if absent |

`--profile auto` selects by target kind and is the default.

Profiles change **expectation and severity only, never measurement**. The same evidence is always collected, so a recorded run can be re-analysed under a different profile without re-running it.

Two signatures follow directly: `SC006 NO_DEREGISTRATION_WINDOW` (closed the listener too early) and `SC005 LISTENER_OPEN_AFTER_WINDOW` (never closed it). They are opposite failures of the same stage, and no single-threshold rule can report both.

## Consequences

- The tool is correct about a subtlety that most documentation, most tutorials, and most hand-rolled scripts get backwards. This is the sharpest differentiator in the product.
- It can teach: `SC006` explains *why* the by-the-book implementation is wrong under Kubernetes, which is more valuable than the pass/fail bit.
- Profiles are a concept users must learn. Mitigated by `auto` selection and by conservative severity defaults — `SC006` is a warning under `standalone` and an error under `lame-duck`/`kubernetes`.
- Because the same evidence supports every profile, an offline re-analysis subcommand becomes nearly free and is worth building.

## Alternatives considered

- **A single configurable tolerance.** Rejected: it cannot express "must accept" and "must refuse" as different requirements over the same window.
- **Strict zero tolerance.** Rejected: it would fail correctly-written Kubernetes services and pass ones that cause 502s. Precisely backwards.
- **Infer the deployment model automatically.** Rejected for v1.0: guessing invisibly conflicts with the "never a false PASS" principle. `auto` infers only from the target kind, which is explicit and inspectable.
