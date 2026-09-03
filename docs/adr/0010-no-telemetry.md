# ADR-0010: No telemetry, ever

- **Status:** Accepted
- **Date:** 2026-09-03

## Context

Usage data would be genuinely useful. Knowing which failure signatures fire most often in the wild would sharpen the remediation catalogue and prioritise the roadmap far better than guesswork.

But ShutdownCheck is intended to run inside CI pipelines and on infrastructure adjacent to production, pointed at internal hostnames, sometimes carrying credentials in request headers.

## Decision

ShutdownCheck collects no telemetry, performs no analytics, and makes no network requests other than to the target the user configured.

No update checks. No anonymous usage counters. No crash reporting. No opt-out flag, because there is nothing to opt out of.

## Consequences

- The tool passes security review trivially. For an infrastructure tool this is close to a precondition for adoption: a binary that phones home from inside a CI runner is a binary that gets blocked, and no feature recovers from that.
- "No telemetry, ever" is a claim anyone can verify by reading the source or watching the process, which makes it a credibility asset rather than a marketing line.
- We give up a real feedback channel. Prioritisation must come from issues, discussions and the conformance suite instead — slower and more biased toward vocal users. Accepted deliberately.
- This constrains implementation: no analytics dependency may be added, and no library that performs background network calls may be introduced. The dependency budget and dependency review help enforce this.
- An `--check-update` style feature would violate this ADR. Adding one would require a new ADR that supersedes it and would be an explicit reversal of a public promise.

## Alternatives considered

- **Opt-in telemetry, disabled by default.** Rejected: it still requires shipping the code path, which still shows up in security review and still requires users to trust a default. The clean claim is worth more than the data.
- **A voluntary "report your results" command.** Not rejected, but out of scope for v1.0. Any such thing must be a separate, obvious, user-initiated action — never automatic.
