# Contributing to ShutdownCheck

Thanks for considering it. This project has an unusual constraint that shapes almost every review comment, so it is worth stating up front:

> **The entire value of this tool is that its verdict can be trusted.**
> A false `PASS` is worse than no tool at all, because it manufactures confidence.
> Changes are reviewed against that standard first, and everything else second.

## Before you start

- Read the **Non-Goals** section of [shutdowncheck-spec.md](shutdowncheck-spec.md). The scope is deliberately narrow, and proposals outside it get closed regardless of quality.
- For anything non-trivial, open an issue first. It is much cheaper to disagree about an approach in an issue than in a large pull request.

## Development setup

Requires Go (minimum version in `go.mod`) and git.

```console
git clone https://github.com/Ashutosh-Panda2004/ShutdownCheck
cd shutdowncheck

make ci             # Linux / macOS
.\make.ps1 ci       # Windows
```

Optional tools used by `make lint` and `make vuln`:

```console
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2
go install golang.org/x/vuln/cmd/govulncheck@v1.7.0
go install github.com/securego/gosec/v2/cmd/gosec@v2.29.0
```

These pinned scanner releases require Go 1.25 or newer even though the project
itself supports the minimum declared in `go.mod`. CI installs them with Go
1.26.8; contributors on the minimum toolchain can rely on those CI jobs.

Both build scripts skip these gracefully if they are not installed; CI runs them regardless.

## Architectural rules that are mechanically enforced

`test/architecture` fails the build if these are violated. They are not style preferences.

1. **`internal/analyze` and `pkg/schema` must stay pure.** No I/O imports, no clock reads. Analysis is `Analyze(Timeline, Policy) -> Report` and nothing else. This is what lets every verdict be tested against a synthetic timeline with no network or subprocess involvement.
2. **`cmd/` holds no logic.** It may wire up `internal/cli`, nothing more.

If you find yourself wanting to break rule 1, the answer is almost always: record the fact you need as an event on the timeline in the package that already does the I/O, then analyse it.

## Testing expectations

| Change | Required tests |
|---|---|
| A new failure signature | At least one positive **and** one negative synthetic-timeline fixture. A registry test fails the build if any signature has no test. |
| Anything affecting a verdict | Proof that determinism holds — the same timeline must analyse to identical output every time. |
| Probe / load / target code | Tests against `httptest` servers or fixture processes, run under `-race`. |
| A renderer | A golden file. |

Run `make race` before opening a pull request. Concurrency bugs in this codebase surface as flaky verdicts, which is the failure mode we can least afford.

## Security rules

These are non-negotiable and are checked in review:

- Build subprocesses as an argv slice. Never use a shell, never interpolate strings into commands.
- Validate any externally-supplied identifier (container name, pod name) before it reaches argv.
- Never let a secret reach a report, a log line or the NDJSON stream. Sensitive headers are redacted at the recording boundary.
- TLS verification stays on unless the user explicitly passed `--insecure`.
- Never signal an invalid PID or shutdowncheck itself. PID 1 is permitted only through the explicit `--allow-unsafe-pid` opt-in for container-init use cases.

Report vulnerabilities privately — see [SECURITY.md](SECURITY.md).

## Dependencies

The direct dependency budget is **fewer than 10**, deliberately. A single static binary that infrastructure teams will run in CI should have a small, auditable supply chain. Adding one requires a justification in the pull request description, and reviewers will push back.

## Commits and pull requests

- [Conventional Commits](https://www.conventionalcommits.org/): `feat:`, `fix:`, `docs:`, `test:`, `refactor:`, `chore:`, `perf:`, `ci:`.
- Keep pull requests focused. A refactor and a behaviour change in one branch is two reviews pretending to be one.
- Fill in the pull request template — particularly the correctness and security sections.

## Decisions

Anything that contradicts the spec, or that a future contributor might reasonably want to reverse, belongs in `docs/adr/` as an ADR. Copy `docs/adr/0000-template.md`. Superseding an old ADR is normal and expected; silently drifting from one is not.

## Code of conduct

By participating you agree to the [Code of Conduct](CODE_OF_CONDUCT.md).
