# Architecture Decision Records

Short documents recording decisions that shaped the project, why they were made, and what they cost.

An ADR is required when a change contradicts [shutdowncheck-spec.md](../../shutdowncheck-spec.md), or when a future contributor might reasonably want to reverse the decision and would need to know what we already considered.

Copy [0000-template.md](0000-template.md) to start a new one. ADRs are immutable once merged — to change a decision, write a new ADR and mark the old one `Superseded by ADR-NNNN`.

| ADR | Decision | Status |
|---|---|---|
| [0001](0001-implementation-language-go.md) | Implement in Go | Accepted |
| [0002](0002-target-interface-and-docker-in-v1.md) | One `Target` interface; Docker ships in v1.0 | Accepted |
| [0003](0003-full-request-spec-not-get-only.md) | Support a full request spec, not GET-only | Accepted |
| [0004](0004-auto-calibrated-load.md) | Auto-calibrate load; no default RPS | Accepted |
| [0005](0005-shutdown-profiles.md) | Shutdown profiles instead of a tolerance window | Accepted |
| [0006](0006-project-name-and-module-path.md) | Keep the name ShutdownCheck; module path | Accepted |
| [0007](0007-enforce-sigkill.md) | Actually send `SIGKILL` at grace expiry | Accepted |
| [0008](0008-windows-support-strategy.md) | Windows via Docker in v1.0; native targets in v1.2 | Accepted |
| [0009](0009-public-versioned-report-schema.md) | The JSON report is a public, versioned contract | Accepted |
| [0010](0010-no-telemetry.md) | No telemetry, ever | Accepted |
| [0011](0011-pure-analysis-core.md) | The analysis core is pure and mechanically enforced | Accepted |
| [0012](0012-cli-without-cobra.md) | Build the CLI on the standard library rather than Cobra | Accepted |
| [0013](0013-demo-subcommand.md) | Ship `demo` as a self-spawned subcommand, with a recorded fallback | Accepted |
