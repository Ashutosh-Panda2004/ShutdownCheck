# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Security

- A target's command line is redacted before it is echoed into a report.
  `./api --db-password=hunter2` is an ordinary way to start a service, and a
  report is uploaded to CI and shared — a longer and wider exposure than the
  process table the value came from. Secret-looking flags, environment
  assignments and URLs carrying tokens are all covered.
- Redaction is now proven end to end. The suite builds a recording that was
  handed credentials by every route the tool records — the probed URL, the
  target's argv, an error string and a captured log line — and asserts none of
  them survive into any output format or the written file. A redaction helper
  that is correct but never called was the failure mode that mattered, and unit
  tests could not see it.
- `--insecure` is recorded in the report as `probe.insecure`, never omitted, so
  a reader can always tell "certificate verification was on" from "this report
  predates the field". It also now prints a warning naming what the verdict does
  and does not cover.
- `--allow-unsafe-pid` permits attaching to PID 1, which is init on a host and
  the service itself inside a container. It never permits signalling
  shutdowncheck's own process: a measurement that destroys the process taking it
  cannot produce a report.
- A wall-clock ceiling bounds the whole run, derived from its own budget or set
  with `--timeout`. A run that hangs forever is worse than one that fails — it
  burns a CI runner until the job is killed and produces no report either way.
- A fuzz target for the report renderers, where recorded evidence meets the
  column arithmetic of the timeline visualisation. 1.9 million executions found
  no crashes.

### Added

- `shutdowncheck demo` runs a real check against a deliberately broken service
  that the binary spawns of itself, so a new user can see a report without
  setting anything up. It is a real process receiving a real signal, measured by
  the ordinary run pipeline: there is no code path in the tool that prints a
  report it did not measure.
- [The seven stages of correct termination](docs/seven-stages.md): the
  conceptual model every check maps onto, including why the usual graceful
  shutdown advice is wrong under Kubernetes.
- `--max-records` caps request evidence, with a larger derived budget for
  connection and observer events. Any omitted verdict-bearing evidence forces
  `INCONCLUSIVE`; captured logs have independent byte, line-length and line-count
  caps.
- Budget tests for the things spec section 19 promises: binary size, startup
  latency, direct dependency count, and verdict stability across repeated runs.
- Release pipeline: GoReleaser builds linux, macOS and Windows on amd64 and
  arm64, with checksums, cosign keyless signatures, an SBOM per archive and
  SLSA build provenance. A tag runs the full test suite before anything is
  published, so a broken binary can never acquire a signature saying it is fine.
- A checksum-verifying install script, a Homebrew tap, a Scoop manifest and a
  distroless multi-arch image on GHCR. The image doubles as its own broken demo
  service, so there is no second image to keep in step.
- Root-discoverable GitHub Action, as a composite action so it adds no image to
  anyone's supply chain. It publishes the original run's JSON report, requires
  an exact binary version, and derives the summary from that same report.
- One published page per failure signature under `docs/signatures/`, generated
  from the catalogue compiled into the binary. The website cannot drift from
  what the tool actually reports, and CI fails if it does.
- Docker target: `shutdowncheck run --docker api --url /healthz` terminates a
  running container under load and reports the same seven stages as a local
  process. Signals go through `docker kill`, the exit code through `docker wait`
  and logs through `docker logs --follow`, all via the CLI rather than the
  Engine SDK so the dependency count stays at one.
- Port-mapping resolution, so a path-only URL is completed from the container's
  published ports rather than requiring a `docker inspect` first. Ambiguity is
  reported with the candidates listed instead of guessed at, because probing an
  admin or metrics listener would draw conclusions about the wrong socket.
- The conformance matrix now runs through the Docker target as well, judged by
  the same assertions as the process target.
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
- `internal/load`: open-model traffic generator with a constant-memory dispatch
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
- Architecture decision records ADR-0001 through ADR-0015 capturing the locked
  decisions from the specification.

### Changed

- Report schema is now `1.1`, adding `probe.insecure`. Additive, per the policy
  in [ADR-0009](docs/adr/0009-public-versioned-report-schema.md).
- `run --help` lists every flag. It previously ended with "run --help to see
  every flag" and then printed itself, which was circular; a test now fails if
  any registered flag is missing from the help.
- A binary from `go install` now reports its real version. There are no ldflags
  on that path, so it falls back to the module version and VCS stamp the
  toolchain embeds instead of calling itself `dev`.
- Container-reference validation is single-sourced in `internal/target`. It was
  implemented twice, once for config files and once for the run path; two copies
  of the same security rule are two copies that can drift apart.
- The end-to-end tests now build the shared conformance server instead of
  keeping a near-identical fixture of their own, and cover the command-line
  surface — formats, file permissions, exit codes, trial aggregation — while
  leaving verdict correctness to the conformance suite.
- `go test -short` skips the suites that spawn and kill real processes, so a
  broken unit test is no longer hidden behind a minute of process wrangling.
  CI runs the full suites in a dedicated job.

### Fixed

- A deep release-readiness audit closed false-PASS paths caused by incomplete
  recordings, unhealthy pre-signal readiness/listener/load baselines, canceled
  observer polls, and fixed-rate runs skipping warmup health checks.
- NDJSON now rejects duplicate keys, unknown fields, invalid enum values,
  impossible timestamps and oversized input. Versioned, sanitized analysis
  context preserves custom gates, severity overrides, target/probe/load metadata
  and multi-trial status for exact default replay.
- The load generator no longer preallocates arrays proportional to
  `rps * duration`; rates, durations, trials, concurrency, report width, request
  bodies, Docker output and evidence retention all have explicit bounds.
- Docker operations are pinned to the immutable container ID returned by
  inspection. Published ports are restricted to validated TCP/IP bindings, and
  Docker readiness ports resolve through the same pinned container state.
- Spawned process cleanup now kills the owned process group even after its
  parent exits, preventing orphan workers from surviving a run.
- The GitHub Action no longer re-analyzes NDJSON and loses custom gates, cannot
  reuse a stale report, rejects invalid gating booleans, paginates sticky-comment
  lookup, and cannot silently install a moving `latest` binary.
- CLI/config overrides that were accepted but ignored now reach runtime;
  conflicting values and malformed methods, headers, URLs, Docker ports, PIDs
  and numeric limits fail before the target is mutated.
- Secret redaction now covers validation/readiness errors, composite query-key
  names, URL fragments, Docker stderr and ANSI-obfuscated names. Terminal and
  bidirectional controls cannot reach human or Markdown output.
- JUnit and badge output can no longer present an inconclusive run as green.
- Release workflows, tools and container bases are immutably pinned; the current
  GoReleaser v2 configuration passes `goreleaser check` and all six release
  archives, checksums, SBOMs and package-manager manifests build locally. Syft
  is pinned to v1.51.1 in the hosted release workflow.
- Two build artefacts had been committed by accident in earlier phases and are
  now removed and ignored.
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
- A container that genuinely exits `137` on its own is indistinguishable from
  one that was `SIGKILL`ed, because the `128 + signal` convention is all the
  Docker interface exposes.
- The orphaned-listener defect (SC012) cannot be reproduced inside a container:
  when PID 1 exits, the daemon reaps everything else with it.

[Unreleased]: https://github.com/Ashutosh-Panda2004/ShutdownCheck/commits/main
