# ADR-0012: Build the CLI on the standard library rather than Cobra

- **Status:** Accepted
- **Date:** 2026-09-03
- **Supersedes:** the implementation note in specification section 7.2, which named Cobra

## Context

Section 7.2 of the specification assumed Cobra for `internal/cli`. Cobra is the default choice for Go command-line tools and brings generated help, subcommand routing and shell completions.

Two constraints pull the other way.

The first is the dependency budget. `CONTRIBUTING.md` states a limit of fewer than ten direct dependencies and tells reviewers to push back on additions. The project currently has one. Cobra adds itself plus `spf13/pflag`, and `inconshreveable/mousetrap` on Windows, taking a third of the budget for argument parsing.

The second is what the budget is for. ShutdownCheck is meant to run inside CI pipelines and next to production infrastructure, and ADR-0010 already trades a useful feature away to keep the security posture simple. A small, auditable supply chain is a feature of this tool rather than an aesthetic preference.

Against that, the actual command surface is small: `run`, `explain`, `validate` and `version`.

## Decision

Implement the CLI with the standard library's `flag` package, using one `flag.FlagSet` per subcommand and hand-written help text.

## Consequences

- The dependency count stays at one, and everything the binary parses is standard library code that a security reviewer already trusts.
- Help text becomes our responsibility. That is a real cost, but the help for a verification tool is worth writing deliberately: it is where the shutdown profiles get explained, and generated output would not have done that anyway.
- Repeated flags such as `--header` and `--fail-on` need small `flag.Value` implementations. This is a handful of lines and is directly unit-testable.
- Shell completions are lost. They were scheduled for the distribution work in Phase 8 and can be provided there as static scripts if there is demand; nothing about this decision blocks that.
- Short-and-long flag pairs (`-o` alongside `--output`) are not automatic. Where a shorthand is genuinely warranted it is registered as a second flag name, which is explicit and costs one line.
- If the command surface grows substantially — nested subcommands, or many more verbs — this should be revisited. That would be a new ADR superseding this one, not a quiet drift back to a framework.

## Alternatives considered

- **Cobra, as originally specified.** Rejected on dependency cost relative to a four-command surface. It remains the right answer for a tool with a large or fast-growing command tree.
- **A smaller third-party parser such as `urfave/cli` or `kong`.** Rejected: still a dependency, still an external supply-chain surface, and the saving over the standard library is small at this size.
- **Writing a bespoke parser.** Rejected outright. `flag` already handles the tedious and easy-to-get-wrong parts, and hand-rolling argument parsing is how tools acquire subtle inconsistencies.
