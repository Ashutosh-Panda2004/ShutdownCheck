# ADR-0003: Support a full request specification, not GET-only

- **Status:** Accepted
- **Date:** 2026-09-03

## Context

The v0.1 draft proposed GET-only load generation for v1, reasoning that the tool's value lies in signal correlation rather than in the load generator, so the generator should stay trivial.

The reasoning is sound but the conclusion does not follow. Drain bugs concentrate on exactly the requests that GET-only traffic cannot produce: slow write paths that hold a transaction open, endpoints that enqueue background work, handlers that take long enough to still be in flight when the signal lands. A `GET /health` that returns in two milliseconds is nearly impossible to catch mid-flight, so a GET-only tool is systematically biased away from the bugs it exists to find.

## Decision

v1.0 supports a full request specification: method, path, headers, and body (inline or from a file). The config file additionally supports a weighted mix of request definitions, so a run can be, for example, 80% fast reads and 20% slow writes.

An optional `--slow-url` names an endpoint used specifically to guarantee long-lived in-flight requests when the primary endpoint is too fast to be caught.

## Consequences

- Removes an entire class of "I cannot use this tool" objections from teams whose interesting endpoints are not GETs.
- Header support means credentials will pass through the tool, which makes redaction a correctness requirement rather than a nicety. Sensitive headers are redacted at the recording boundary, and bodies are never echoed into reports — only a length and a hash.
- Weighted scenarios mean the report must attribute findings per request definition; a drain bug on the write path must not be diluted by a healthy read path.
- Modest extra complexity in the load generator and config model. This is accepted; the cost is contained and one-time.

## Alternatives considered

- **GET-only in v1, extend in v1.1.** Rejected: it would ship a v1 that is biased against finding real bugs, and first impressions of a verification tool are hard to repair.
- **Full scripting, in the style of k6.** Rejected as scope creep. A declarative request specification covers the realistic cases; a scripting engine would make this a load testing tool, which the non-goals explicitly forbid.
