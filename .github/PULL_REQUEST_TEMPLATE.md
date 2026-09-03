## What and why

<!-- What does this change, and what problem does it solve? Link the issue. -->

Closes #

## Definition of Done

- [ ] New behaviour is covered by tests; the phase's coverage target still holds
- [ ] `make ci` (or `.\make.ps1 ci`) passes locally
- [ ] No new direct dependency, or one is added with a justification below
- [ ] Public behaviour is documented (README, CLI help, or `docs/`)
- [ ] Any deviation from `shutdowncheck-spec.md` is recorded as an ADR in `docs/adr/`

## Correctness of verdicts

<!-- Delete this section if the change cannot affect a verdict. -->

- [ ] Every signature touched has both a positive and a negative fixture
- [ ] No change makes a `PASS` reachable in a situation the tool cannot actually prove
- [ ] Determinism holds: repeated analysis of the same timeline yields identical output

## Security

- [ ] No shell string interpolation; subprocesses are built as argv slices
- [ ] Externally-supplied identifiers are validated before reaching argv
- [ ] No secrets or unredacted headers can reach reports, logs or NDJSON
- [ ] TLS verification remains on unless the user explicitly opted out

## New dependency justification

<!-- Required only if go.mod gained a direct dependency. The budget is <10. -->
