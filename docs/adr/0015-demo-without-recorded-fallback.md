# ADR-0015: Drop the recorded demo fallback in favour of an honest refusal

- **Status:** Accepted
- **Date:** 2026-09-04
- **Amends:** [ADR-0013](0013-demo-subcommand.md), whose self-spawn decision stands; only the fallback is replaced

## Context

ADR-0013 decided that `shutdowncheck demo` would re-execute the binary through a hidden subcommand and run the ordinary pipeline against it, and that on platforms without signals it would instead analyse a recorded run shipped alongside the binary, clearly labelled as a recording.

The self-spawn half survived contact with implementation. The fallback did not.

The recording has to be a genuine measurement. ADR-0013 says so explicitly, and states that it must be produced by the conformance suite rather than hand-written, precisely so that the demo never shows evidence that was not measured.

That requirement cannot be satisfied by the machine that needs the fallback. Producing a real recording requires running a real process target under real signals — exactly the capability Windows lacks. So the recording must be generated elsewhere and committed, and until it is, `go:embed` fails to compile or the demo ships broken.

The obvious shortcut is to synthesise a timeline from the analysis layer's test fixtures. That would be fabricated evidence presented as a measurement, in the one command whose entire purpose is convincing a new user that the tool's output can be trusted. It is precisely what ADR-0013 was written to prevent.

Since ADR-0013 was written, Phase 7 shipped the Docker target. A container on Windows receives a real `SIGTERM` from a real daemon, so the platform can host a genuine demo after all — just not an in-process one.

## Decision

Keep the self-spawn demo from ADR-0013 unchanged on platforms that have signals.

Where they do not, `demo` neither simulates nor replays. It explains in one short paragraph why an in-process demo is impossible on this platform, prints the exact `--docker` invocation that produces the same demonstration against a real container, and exits `4` — the code that already means "the tool could not run", as distinct from a verdict.

## Consequences

- Nothing is embedded, nothing is generated, and no artefact has to be regenerated when the timeline schema changes. The maintenance burden ADR-0013 accepted disappears with it.
- There is no code path anywhere in the tool that prints a report it did not measure. That is a stronger property than ADR-0013 achieved and is worth more than the fallback was.
- Windows users get a non-zero exit from `demo`. This is the honest outcome: no demonstration happened. The message carries the command that will produce one, so the dead end is one line long.
- The Docker route needs an image and therefore a network pull, which is worse than the offline demo Unix users get. The asymmetry is real and is a consequence of the platform, not of this decision.
- If native Windows termination lands in v1.2 as ADR-0008 anticipates, the fallback stops being needed at all rather than becoming another thing to keep working.

## Alternatives considered

- **Generate the recording in CI and commit it.** Rejected for now: it makes the demo depend on an artefact that cannot be produced or verified on the machine doing the work, and a stale recording would misrepresent current behaviour while looking authoritative. Reconsider if a genuinely useful offline demo is wanted on signal-less platforms.
- **Synthesise the timeline from analysis fixtures.** Rejected outright. Fabricated evidence in the trust-building command is the worst possible place for it.
- **Have `demo` transparently fall back to Docker when a daemon is present.** Tempting, and close to being right. Rejected because a demo that silently changes mechanism is a demo that teaches the wrong thing about what was measured. Printing the command keeps the user in control and shows them the invocation they will need for their own service anyway.
