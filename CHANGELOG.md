# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Cross-language conformance suite (`test/conformance/`): the same thirteen
  shutdown defects implemented in Go, Node.js and Python, each asserted to
  produce the same verdict and the same signatures. This is where "works
  regardless of your stack" stops being a claim. `correct` is the most
  important case in the suite — anything can report failures, but only a tool
  that clears a genuinely correct service is worth putting in CI.
- A build-failing guard requiring every asserted signature to be exercised in at
  least two independent stacks, with a single documented exemption for SC012,
  which is a defect of process topology rather than of any framework. The
  exemption is itself tested and fails once it is no longer needed.
- A working command line: `run`, `analyze`, `explain`, `validate` and `version`,
  with the exit-code taxonomy from the specification so a pipeline can tell a
  detected defect from a misconfigured invocation.
- `shutdowncheck analyze run.ndjson --profile kubernetes` re-judges a recorded
  run without repeating it. The same evidence can pass standalone and fail
  behind a load balancer, which is the point of separating measurement from
  interpretation.
- `internal/report`: terminal output with a timeline that puts traffic,
  readiness, the listener, the process and the grace budget on one shared axis,
  plus JSON, JUnit XML, Markdown, NDJSON and an SVG score badge. Colour is
  disabled automatically off a terminal and honours NO_COLOR.
- `internal/remediate`: stack fingerprinting and embedded documentation, with a
  generated page for any signature that has no hand-written one, so no finding
  is ever left unexplained.
- `internal/analyze`: the diagnosis engine. Phase classification with strict
  boundaries, all eighteen failure signatures as independent rules returning
  structured evidence, profile-aware severity, gate evaluation, the 0-100
  shutdown score with its full derivation, and multi-trial aggregation where a
  failure outranks an inconclusive run and a pass can override neither.
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
- `internal/config`: `shutdowncheck.yaml` parsing with strict unknown-field
  rejection, multi-problem validation, and resolution into a runnable scenario.
- Fuzz targets for the NDJSON and YAML parsers.
- Phase 0 scaffold: Go module, package skeleton, and a `version` subcommand.
- Architecture guard tests that fail the build if `internal/analyze`,
  `internal/timeline` or `pkg/schema` gain an I/O dependency or read the clock,
  and if `cmd/` imports anything other than `internal/cli`.
- A registry guard that fails the build if any signature lacks both a positive
  and a negative fixture, so the guarantee cannot rot as rules are added.
- Build tooling: `Makefile` and a PowerShell equivalent (`make.ps1`) with
  matching targets.
- CI: build, test and race across Linux, macOS and Windows; coverage;
  `gofmt`/`go vet`/`golangci-lint`; `go mod tidy` verification; `govulncheck`;
  `gosec`; CodeQL; dependency review; Dependabot.
- Governance: Apache-2.0 licence, contributing guide, code of conduct, security
  policy, issue templates (including a misdiagnosis template that collects the
  raw timeline) and a pull request template.
- Architecture decision records ADR-0001 through ADR-0013 capturing the locked
  decisions from the specification.

### Changed

- The end-to-end tests now build the shared conformance server instead of
  keeping a near-identical fixture of their own, and cover the command-line
  surface — formats, file permissions, exit codes, trial aggregation — while
  leaving verdict correctness to the conformance suite.
- `go test -short` skips the suites that spawn and kill real processes, so a
  broken unit test is no longer hidden behind a minute of process wrangling.
  CI runs the full suites in a dedicated job.

### Fixed

- The orphaned-listener fixture re-bound the port instead of inheriting the
  listening socket, so it would have failed with "address already in use" and
  reproduced nothing. The child now receives the socket as a file descriptor,
  which is how the defect actually occurs.
- A missing language runtime silently skipped part of the conformance matrix.
  It is now a hard failure when `CI` is set: a shrinking matrix must not be able
  to hide behind a green tick.
- A config file containing an empty scenario stub crashed validation with a nil
  pointer dereference. Found by fuzzing; the crashing input is committed as a
  regression seed.

### Known limitations

- Process and managed-command targets require POSIX signals and are therefore
  unavailable on Windows, where they fail with an explicit error pointing at the
  Docker target. See `docs/adr/0008-windows-support-strategy.md`.
- The conformance suite covers Go, Node.js and Python. Java and C# are deferred
  until they can be verified on a machine with those toolchains rather than
  written blind.

[Unreleased]: https://github.com/shutdowncheck/shutdowncheck/commits/main
