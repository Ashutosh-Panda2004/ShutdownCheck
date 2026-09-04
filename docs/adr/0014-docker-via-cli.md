# ADR-0014: Drive Docker through the CLI rather than the Engine SDK

- **Status:** Accepted
- **Date:** 2026-09-04
- **Relates to:** [ADR-0002](0002-target-interface-and-docker-in-v1.md), which put Docker behind the `Target` interface from day one

## Context

Phase 7 adds a Docker target: signal a container, watch it die, capture its logs. There are two ways to talk to a Docker daemon.

The official Go SDK, `github.com/docker/docker/client`, is the obvious choice on paper. It is typed, it handles the API version negotiation, and it speaks directly to the socket without a subprocess.

It is also enormous. Pulling it in drags in the bulk of the Docker codebase — `docker/distribution`, `opencontainers/image-spec`, `containerd` fragments, gRPC, and a long tail of transitive modules. `CONTRIBUTING.md` sets a budget of fewer than ten direct dependencies, and the project currently has one. This single import would take the module graph from roughly a dozen packages to several hundred, and every one of them lands in the supply chain of a tool that people are being asked to run next to production.

The alternative is to shell out to the `docker` binary, which is what the specification already assumed in section 6.

Two things make shelling out more attractive here than it usually is.

First, the surface is genuinely tiny: kill, inspect, wait, logs. Four commands, all with stable output contracts, and `docker inspect` emits JSON specifically so that it can be parsed.

Second, the `docker` CLI is already how the daemon gets reached in practice. It resolves `DOCKER_HOST`, contexts, TLS material, credential helpers and rootless socket paths. Reimplementing that resolution against the SDK is work, and getting it subtly wrong means the tool fails on exactly the non-default setups that are hardest for a user to debug.

The real objection to subprocesses is argument injection, and it is a serious one here because a container reference arrives from a flag and ends up in `argv`.

## Decision

Talk to Docker by executing the `docker` binary directly — never through a shell, never with an interpolated command string.

Container references are validated against a strict allowlist before they reach `argv`, and the whole Docker surface sits behind a narrow interface inside `internal/target` so the rest of the codebase cannot tell the difference and the tests do not need a daemon.

## Consequences

- The dependency count stays at one. Everything that parses untrusted daemon output is standard-library JSON.
- `DOCKER_HOST`, contexts, TLS and rootless setups work because the CLI resolves them. The tool inherits that for free and stays correct as those mechanisms evolve.
- A `docker` binary must be on `PATH`. Its absence is a first-class, actionable error carrying exit code `4` — "the tool could not run" — rather than a stack trace or a misleading verdict.
- Container references must match `^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`. The leading-character rule is the load-bearing part: a reference beginning with `-` would be parsed by the CLI as a flag, which is the argument-injection path this validation exists to close. Docker's own naming rules are a subset of this, so nothing legitimate is rejected.
- Output parsing is a coupling to the CLI's contract. It is confined to one file, uses `--format` with explicit JSON rather than scraping human-readable text, and is covered by tests built from recorded real output.
- Exit codes come back through `docker wait` and follow the `128 + signal` convention. A container that genuinely exits `137` on its own is indistinguishable from one that was `SIGKILL`ed. This is a limitation of the interface Docker exposes, it is documented where the mapping happens, and it matters because misreading a kill as a deliberate non-zero exit would raise SC013 when the real finding is SC002.
- Each call costs a process spawn. At four calls per run plus one long-lived `docker logs --follow`, this is irrelevant next to a grace period measured in seconds.
- If the tool ever needs streaming events, attach, or per-connection detail the CLI does not expose, this should be revisited. Kubernetes in v1.1 is a separate decision and does not inherit from this one; `kubectl` is a much weaker contract than `docker inspect --format`.

## Alternatives considered

- **The official Docker Engine SDK.** Rejected on supply-chain cost. Several hundred transitive packages to send four commands is a bad trade for a tool whose security posture is part of its value.
- **Hand-rolled HTTP against the Engine API socket.** Tempting — the API is well documented and needs no dependencies. Rejected because it means reimplementing `DOCKER_HOST` parsing, context resolution, TLS and named-pipe transport, and each of those is a way to fail on someone's machine for reasons they cannot diagnose. Worth reconsidering if CLI parsing ever becomes the larger burden.
- **Requiring the user to pass a container PID and reusing the process target.** Rejected: it only works when the daemon shares a PID namespace with the tool, which is false on macOS, Windows and every remote daemon, and it cannot observe container exit codes at all.
