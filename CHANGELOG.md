# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Phase 0 scaffold: Go module, package skeleton, and a `version` subcommand.
- Architecture guard tests that fail the build if `internal/analyze` or
  `pkg/schema` gain an I/O dependency or read the clock, and if `cmd/` imports
  anything other than `internal/cli`.
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

[Unreleased]: https://github.com/shutdowncheck/shutdowncheck/commits/main
