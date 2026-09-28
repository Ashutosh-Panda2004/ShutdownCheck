# ADR-0006: Project name and Go module path

- **Status:** Accepted
- **Date:** 2026-09-03

## Context

The name affects discoverability, the module path, the binary name, the documentation domain, and every import in the repository. Changing it later is mechanical but touches every file, so it is worth deciding before the first public commit.

"ShutdownCheck" is descriptive rather than distinctive. Distinctive names are more memorable; descriptive names are more findable.

## Decision

Keep **ShutdownCheck**. The binary is `shutdowncheck`, with `sdc` as a shorthand alias.

The Go module path is:

```
github.com/Ashutosh-Panda2004/ShutdownCheck
```

## Consequences

- For a niche infrastructure tool, literal problem-name discoverability outweighs memorability. People experiencing this bug search for what it does, not for a clever name, and the long-tail documentation pages (one per failure signature) compound that advantage.
- The `shutdowncheck` GitHub organisation and the `shutdowncheck.dev` domain **must be reserved before the repository is made public**. This is a prerequisite, not a follow-up.
- Availability must be verified on GitHub, Homebrew, npm, crates.io and PyPI. Not to publish everywhere, but to avoid colliding with an existing tool that means something different.
- Until the organisation is confirmed, the module path is provisional. If it changes, the fix is a single find-and-replace across import paths plus `go mod edit -module`; no code changes are required. `.golangci.yml` also references the path for `goimports` local-prefix grouping.

## Alternatives considered

- **`drainkit`, `lameduck`, `terminus-check`.** More distinctive and retained as a fallback shortlist if the name proves to be taken. Rejected as the primary choice because they lose the direct match against how people describe the problem.
- **Publishing under a personal account.** Rejected: an organisation makes future co-maintainership straightforward and signals that the project is intended to outlive one person's interest.
