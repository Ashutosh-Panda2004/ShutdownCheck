# ADR-0013: Ship `demo` as a self-spawned subcommand, with a recorded fallback

- **Status:** Accepted
- **Date:** 2026-09-03
- **Resolves:** open question 5 in specification section 20
- **Implementation:** Phase 8, alongside the distribution work

## Context

A verification tool has a credibility problem on first contact. Someone who has just installed ShutdownCheck has no broken service to point it at, and no reason yet to believe the output means anything. Asking them to write a deliberately faulty server before they can see a single report is a good way to lose them at the first step.

`shutdowncheck demo` is meant to close that gap: one command, no arguments, that produces a real report against a service with a real defect.

The question left open in section 20 was how to obtain that service. Three routes were considered, and the deciding constraint is that a demo which fakes its evidence would undermine the exact property the tool exists to provide. If the demo prints a report that was not measured, then nothing the tool prints can be taken at face value.

A second constraint is platform. ADR-0008 established that ShutdownCheck refuses process targets on Windows rather than pretending, because Windows has no `SIGTERM` and simulating one would produce a verdict about a signal that was never delivered. A live demo therefore cannot run there — and the demo is precisely the moment when failing with a bare "unsupported platform" does the most damage.

## Decision

Implement `demo` as a self-spawn: the binary re-executes itself through a hidden `__demo-server` subcommand, resolved with `os.Executable()`, and runs the ordinary `run` pipeline against it.

The demo target is a real process, receives a real signal, and is measured by exactly the same code path as any other target. The report is genuine.

Where signals are unavailable — Windows today — `demo` does not fail and does not simulate. It analyses a recorded run shipped with the binary, prints the report, and states plainly at the top that it is a recording rather than a live measurement.

## Consequences

- No embedded server binary, no downloads, no Docker requirement. The demo works offline and adds nothing to the release artefacts beyond one NDJSON file.
- The demo exercises the production path. If `demo` breaks, something real is broken, which makes it useful as a smoke test rather than a separate thing to maintain.
- The hidden subcommand is an undocumented surface. It is prefixed with `__`, omitted from help, and must refuse to run unless invoked with the argument shape `demo` produces, so it cannot become an accidental dependency of anyone's scripts.
- Re-executing the current binary is the same mechanism ADR-0008's platform check already guards, so the Windows path is a deliberate branch rather than a surprise at runtime.
- The recorded fallback needs the "this is a recording" banner to be impossible to miss. A recorded report that reads as a live one would be the fake evidence this decision exists to avoid.
- The bundled NDJSON must be regenerated when the timeline schema changes. It is produced by the conformance suite rather than hand-written, and a test asserts it still parses and still yields the verdict the demo claims.
- Windows users get a weaker first experience than Unix users. That is an honest reflection of a real platform limitation, and it is preferable to a demo that quietly reports on a signal the operating system never delivered.

## Alternatives considered

- **Embed a prebuilt demo server in the binary.** Rejected. It multiplies release artefacts across every platform, inflates the binary, and adds a second thing that must be signed and kept in step with the tool.
- **Require Docker for the demo.** Rejected. The demo exists to be the lowest-friction possible first run, and gating it behind a container runtime inverts that. Docker targets are Phase 7 and are a separate concern.
- **Have the demo print a canned report on every platform.** Rejected outright. It would be fabricated evidence in the one place users are deciding whether to trust the tool at all.
- **Omit `demo` entirely.** Rejected. The onboarding gap is real, and "write a broken service first" is a worse answer than a well-scoped demo.
