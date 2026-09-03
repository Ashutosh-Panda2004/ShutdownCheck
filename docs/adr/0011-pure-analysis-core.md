# ADR-0011: The analysis core is pure, and that is mechanically enforced

- **Status:** Accepted
- **Date:** 2026-09-03

## Context

The entire value of ShutdownCheck rests on one property: **its verdict can be trusted**. A false `PASS` is worse than having no tool, because it manufactures confidence in a service that drops traffic on every deploy.

Trust here reduces to two testable properties. The verdict must be *deterministic* — the same evidence must always produce the same answer — and it must be *cheaply testable*, because a rule engine covering eighteen failure signatures across five shutdown profiles has far too many combinations to verify through live end-to-end runs alone.

Both properties are destroyed by the same thing: analysis code that reaches out to the world. A rule that calls `time.Now()` is not reproducible. A rule that opens a socket cannot be tested from a fixture. And this kind of coupling never arrives deliberately; it arrives as one pragmatic shortcut under deadline pressure, and afterwards nobody can tell which verdicts are trustworthy.

## Decision

Analysis is a pure function:

```go
Analyze(timeline.Timeline, Policy) -> schema.Report
```

`internal/analyze` and `pkg/schema` must perform **no I/O** and must **never read the clock**. All observation happens in the probe, load and target packages, which record evidence onto the timeline. Analysis then operates only on that immutable record.

This is enforced mechanically by `test/architecture`, which fails the build when:

1. A guarded package reaches a denied import — transitively, not merely directly, so I/O cannot be laundered through an internal helper.
2. A guarded package calls `time.Now`, `time.Since`, `time.Sleep`, `time.After`, or any other clock-reading or clock-waiting entry point. `time.Duration` and `time.Time` *values* remain permitted; only reading the clock is forbidden.

A companion test keeps `cmd/` free of logic: it may import `internal/cli` and nothing else.

The denylist logic is itself unit-tested, so a broken guard cannot silently degrade into a no-op that everyone assumes is protecting them.

## Consequences

- Every failure signature is testable against a synthetic timeline constructed in code, with no network, no subprocess and no sleeping. This is what makes the requirement of a positive *and* a negative fixture per signature realistic rather than aspirational.
- Determinism becomes a property that can be asserted: analysing the same timeline fifty times must produce byte-identical output.
- Recorded runs can be re-analysed offline under a different shutdown profile without re-running the experiment, because measurement and interpretation are cleanly separated. ADR-0005 depends on this.
- Some genuine friction is accepted. Anything analysis needs must first be recorded as a timeline event by a package that already performs I/O. When that feels awkward, the right response is to extend the event model — not to weaken the guard.
- The guard is deliberately hard to bypass. Suppressing it should require a visible, reviewable change to `test/architecture`, and any such change is an ADR-level decision.

## Alternatives considered

- **A documented convention with no enforcement.** Rejected: conventions decay exactly when they matter most, and the decay is invisible until a verdict is already wrong.
- **Injecting a `Clock` interface into the analyzer instead of forbidding clock access.** Rejected as strictly weaker: it permits time-dependent logic to exist at all. Analysis has no legitimate reason to know the current time, because every timestamp it needs is already on the timeline.
- **Direct-import checking only.** Rejected: a single internal helper package would defeat it. The transitive closure is only a few dozen lines more and closes the loophole properly.
