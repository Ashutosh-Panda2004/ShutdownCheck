# ADR-0009: The JSON report is a public, versioned contract

- **Status:** Accepted
- **Date:** 2026-09-03

## Context

The tool's primary use is as a CI gate, so its JSON output will be parsed by scripts, dashboards and other tools almost immediately. Output that is treated as an internal implementation detail gets restructured casually, and every downstream integration breaks silently.

## Decision

The report types live in `pkg/schema` — a **public** package, deliberately outside `internal/`, so third parties can import them as typed Go structures rather than reverse-engineering field names.

Every report carries a `schema_version`. Additive changes bump the minor version. Removing a field, renaming it, or changing its meaning requires a major bump **and** a new ADR.

The scoring weight table is versioned alongside the schema, so any published score can be reproduced and audited.

## Consequences

- Field naming, nullability and units become review-level decisions. Durations are named with an explicit `_ms` suffix, and absent measurements are `null` rather than `0` — the difference between "readiness flipped immediately" and "readiness never flipped" must never be ambiguous.
- `pkg/schema` inherits the purity constraint from ADR-0011: no I/O, no clock reads. It is a data contract, not a library with behaviour.
- Golden-file tests guard the serialised form, so an accidental field rename fails CI rather than silently breaking consumers.
- Some flexibility is lost. A field that turns out to be badly designed cannot simply be dropped. This is the intended cost of being depended upon.

## Alternatives considered

- **Keep the report internal and treat JSON as best-effort.** Rejected: it guarantees breakage for exactly the integrations that indicate the project is succeeding.
- **Publish a JSON Schema document instead of Go types.** Not rejected, but insufficient alone. Go types are the source of truth; generating a JSON Schema document from them is a reasonable later addition for non-Go consumers.
