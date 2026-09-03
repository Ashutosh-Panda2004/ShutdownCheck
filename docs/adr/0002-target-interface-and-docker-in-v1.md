# ADR-0002: One Target interface, with Docker shipping in v1.0

- **Status:** Accepted
- **Date:** 2026-09-03

## Context

The tool must terminate something. That something might be a bare process, a process it launches itself, a Docker container, a Compose service, or a Kubernetes pod. The v0.1 draft treated Docker support as an open question about scope: include it in the MVP or defer it.

That framing was wrong. The scope question that matters is not "Docker or not" but "is target handling behind an abstraction or scattered through the codebase". If it is scattered, every new target kind becomes an invasive change and the tool ossifies around local processes.

## Decision

Define a single `Target` interface covering start, readiness, signalling, liveness, exit status, log capture, default grace period, and cleanup. Every target kind implements it, and no other package branches on target kind.

Ship three implementations in v1.0:

- `ProcessTarget` — attach by PID
- `CommandTarget` — shutdowncheck spawns and owns the process
- `DockerTarget` — a running container

Kubernetes and Compose follow in v1.1, adding no changes outside `internal/target`.

## Consequences

- `DockerTarget` becomes a small, contained implementation (`docker kill --signal`, `docker inspect`, `docker logs --follow`) rather than a cross-cutting feature.
- Target-specific behaviour that genuinely differs — a 10s default grace period for Docker versus 30s for process and Kubernetes semantics — is expressed through the interface rather than by conditionals in the orchestrator.
- Docker targets bring a real external dependency: the tool must degrade with a clear message and exit code `4` when the Docker CLI or daemon is unavailable, rather than failing obscurely.
- Container and pod identifiers arrive from user input and reach a subprocess argv, so they must be validated against a strict allowlist regex. This is recorded here because it is easy to forget when adding the next target kind.

## Alternatives considered

- **Process-only in v1.0, add targets later.** Rejected: it invites the abstraction to be retrofitted under deadline pressure, which is exactly when it gets done badly.
- **Use the Docker Engine API via a client library instead of the CLI.** Rejected for v1.0: it adds a large dependency against a strict budget, and the CLI is already present wherever containers are being used. Revisit if CLI parsing proves fragile.
