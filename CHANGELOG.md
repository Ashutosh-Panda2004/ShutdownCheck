# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `internal/timeline`: the immutable event model every observer writes into and
  analysis reads from, with a concurrency-safe bounded recorder and a
  round-trippable NDJSON codec for offline re-analysis.
- `internal/clock`: a `Clock` interface with a manually-advanced fake, so
  schedulers and monitors can be tested without sleeping.
- `pkg/schema`: the public, versioned report contract, guarded by a golden file.
  Absent measurements serialise as `null` rather than `0`.
- `internal/analyze`: the 18-entry failure signature catalogue and the shutdown
  `Policy`/`Profile` model. Detection rules follow in Phase 4.
- `internal/config`: `shutdowncheck.yaml` parsing with strict unknown-field
  rejection, multi-problem validation, and resolution into a runnable scenario.
- Fuzz targets for the NDJSON and YAML parsers.
- Phase 0 scaffold: Go module, package skeleton, and a `version` subcommand.
- Architecture guard tests that fail the build if `internal/analyze`,
  `internal/timeline` or `pkg/schema` gain an I/O dependency or read the clock,
  and if `cmd/` imports anything other than `internal/cli`.
- Build tooling: `Makefile` and a PowerShell equivalent (`make.ps1`) with
  matching targets.
- CI: build, test and race across Linux, macOS and Windows; coverage;
  `gofmt`/`go vet`/`golangci-lint`; `go mod tidy` verification; `govulncheck`;
  `gosec`; CodeQL; dependency review; Dependabot.
- Governance: Apache-2.0 licence, contributing guide, code of conduct, security
  policy, issue templates (including a misdiagnosis template that collects the
  raw timeline) and a pull request template.
- Architecture decision records ADR-0001 through ADR-0011 capturing the locked
  decisions from the specification.

### Fixed

- A config file containing an empty scenario stub crashed validation with a nil
  pointer dereference. Found by fuzzing; the crashing input is committed as a
  regression seed.

[Unreleased]: https://github.com/shutdowncheck/shutdowncheck/commits/main
