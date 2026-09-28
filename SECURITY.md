# Security Policy

## Reporting a vulnerability

**Please do not open a public issue for a security problem.**

Report privately via GitHub's [private vulnerability reporting](https://github.com/Ashutosh-Panda2004/ShutdownCheck/security/advisories/new) on this repository.

Please include: what the issue is, how to reproduce it, the version affected, and what an attacker could achieve. We will acknowledge the report, keep you updated while we investigate, and credit you in the advisory unless you would rather we did not.

Please give us a reasonable window to release a fix before disclosing publicly.

## Supported versions

The project is pre-1.0. Until v1.0.0 ships, only the latest release receives fixes. After v1.0.0 this section will state a concrete support window.

## What this tool does to your system

ShutdownCheck is designed to be run in CI next to real services, so it is worth being explicit about the powers it holds:

- **It sends signals to processes**, including `SIGKILL` at grace expiry.
- **It can spawn and own a process group**, and kills that whole group during cleanup.
- **It sends HTTP traffic** at a calibrated rate to a URL you supply.
- **It shells out to container runtimes** (`docker`) for container targets.

Guarantees that constrain all of the above:

| Area | Guarantee |
|---|---|
| Command execution | Subprocesses are built as an argv slice and executed directly. No shell is ever invoked and no string interpolation occurs. Externally-supplied identifiers are validated against a strict allowlist before reaching argv. |
| Signal targets | Refuses invalid PIDs and shutdowncheck's own process unconditionally. PID 1 requires the explicit `--allow-unsafe-pid` opt-in because it is init on a host but can be the service inside a container. `SIGKILL` only ever reaches a target the tool positively identified. |
| Process cleanup | Spawned process groups are terminated on every exit path, including panic and interruption of the tool itself. |
| TLS | Certificate verification is on by default. `--insecure` is explicit, prints a warning, and is recorded in the report. |
| Egress | Traffic goes only to the configured target. There are no update checks, no analytics, and **no telemetry of any kind**. |
| Secrets | Sensitive headers, secret-looking URL query values, command-line arguments and captured log fields are redacted in reports and NDJSON. Request headers and bodies are not recorded at all. |
| Report files | Written atomically with `0600` permissions on POSIX systems, since they may contain internal hostnames and URLs. Windows inherits the destination directory's ACL because it has no POSIX mode bits. |
| Resource limits | Request and auxiliary evidence, captured target logs, config/evidence/request/response bodies, rates, durations, trials, report width, Docker output and open sockets are bounded. If verdict-bearing evidence is omitted, the result is `INCONCLUSIVE`, never `PASS`. |

If you find a way to break any of these guarantees, that is a vulnerability — please report it.

## Supply chain

- Minimal, pinned dependencies with a committed `go.sum`; the direct dependency budget is deliberately under 10.
- Every pull request runs `go vet`, `golangci-lint`, `gosec`, `govulncheck`, CodeQL, dependency review, and the full test suite under the race detector.
- Releases will be reproducible, ship an SBOM, and be signed with `cosign` with SLSA provenance.

## Out of scope

- Findings that require an attacker to already control the machine running ShutdownCheck.
- The fact that the tool terminates the process you explicitly told it to terminate.
- Load intentionally imposed on the selected target within the documented rate
	and concurrency limits.
