# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `internal/target`: the `Target` interface plus process and managed-command
  implementations, with process-group ownership, readiness gating that fails
  fast when a target dies during startup, guaranteed idempotent cleanup, and
  safety guards that refuse PID 1 and shutdowncheck's own process.
- `internal/run`: the run state machine, which drives preflight, warmup,
  calibration, steady state, the optional pre-stop hold, signal delivery with
  skew measurement, SIGKILL escalation at grace expiry, and the post-exit port
  check. Traffic runs as one continuous phase spanning the signal.
- Target log capture with per-line and total byte caps and secret redaction.
- `internal/redact`: extracted from `internal/probe` now that probing, target log
  capture and reporting all need it.
- `internal/probe`: instrumented HTTP prober built on `httptrace`, with
  connection-layer forensics (reuse, `Connection: close`, and FIN vs RST
  termination), an error classifier that separates refused, reset, timeout, EOF,
  TLS and DNS failures, and independent readiness and raw-TCP listener probes.
- `internal/load`: open-model traffic generator with a precomputed dispatch
  schedule, bounded concurrency with measured back-pressure, deterministic
  weighted request selection, and Little's Law calibration that marks the run
  unachievable rather than silently running at the ceiling.
- `internal/testutil`: goroutine-leak detection for tests, with no third-party
  dependency.
- Request events now carry their scheduled dispatch time, so queueing inside the
  generator is measurable and distinguishable from service latency.
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

### Known limitations

- Process and managed-command targets require POSIX signals and are therefore
  unavailable on Windows, where they fail with an explicit error pointing at the
  Docker target. See `docs/adr/0008-windows-support-strategy.md`.

[Unreleased]: https://github.com/shutdowncheck/shutdowncheck/commits/main
