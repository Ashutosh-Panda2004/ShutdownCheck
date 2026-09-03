# End-to-end tests

Table-driven tests that run the real binary against real processes and
containers, asserting `(target kind, language, variant) -> expected signatures
and verdict`.

These are the slowest and most valuable tests in the project: they are the only
ones that exercise signal delivery, process-group cleanup and connection
forensics against genuinely independent servers.

Also home to the robustness suites required before v1.0:

- **Soak** — 200 consecutive runs asserting zero leaked processes, sockets or
  goroutines.
- **Determinism** — the same fixture analysed repeatedly must yield an identical
  verdict every time. A flaky gate is a gate that gets deleted.

Started in Phase 3, expanded in Phase 6.
