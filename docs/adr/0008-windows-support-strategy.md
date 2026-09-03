# ADR-0008: Windows support via Docker in v1.0; native targets in v1.2

- **Status:** Accepted
- **Date:** 2026-09-03

## Context

Windows has no `SIGTERM`. Graceful termination there uses a different model entirely: console control events (`CTRL_C_EVENT`, `CTRL_BREAK_EVENT`), window messages, service control manager stop requests, and job objects. Delivering a console control event to another process requires attaching to its console, which carries constraints that have no POSIX equivalent.

A meaningful share of the tool's likely audience develops on Windows — notably .NET teams, who deploy to Linux containers but write code on Windows desktops.

## Decision

Three-part decision:

1. The **shutdowncheck binary builds, tests and runs on Windows from v1.0**. CI includes `windows-latest` for build, test and race from Phase 0.
2. **Docker targets work on Windows in v1.0.** The signal is delivered by the Docker daemon to a Linux container, so the POSIX semantics are intact and the Windows host is only orchestrating. This covers the realistic case: a Windows developer testing the container they will actually deploy.
3. **Native Windows process targets are deferred to v1.2**, and will be designed properly rather than approximated.

The `Signal` abstraction in the `Target` interface is defined so that adding a Windows termination model is additive.

## Consequences

- Windows developers get real value in v1.0 without the project pretending to a fidelity it has not earned.
- `ProcessTarget` and `CommandTarget` must fail on Windows with an explicit, honest message pointing at the Docker target — not with an obscure syscall error, and never by silently substituting a hard kill for a graceful one. A hard kill masquerading as `SIGTERM` would produce a fabricated verdict, which is the one thing this project will not do.
- Cross-platform CI from day one means Windows path handling, process group semantics and line endings are exercised continuously instead of being discovered late.
- The v1.2 work is genuine design effort, not a port: console control events, job objects and service stop requests are three different mechanisms with different applicability.

## Alternatives considered

- **Map `SIGTERM` to `TerminateProcess` on Windows.** Rejected outright. `TerminateProcess` is the equivalent of `SIGKILL`; presenting it as graceful termination would make every Windows verdict a lie.
- **Drop Windows support entirely.** Rejected: it excludes a large part of the audience, and the binary itself is cross-platform for free.
- **Full native Windows support in v1.0.** Rejected: it would delay v1.0 substantially for a model that is genuinely different, while the Docker path already serves the common case.
