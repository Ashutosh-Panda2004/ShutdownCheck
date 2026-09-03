# Timeline fixtures

Synthetic timelines used to test the analysis engine without any network or
subprocess involvement. This is only possible because `internal/analyze` is a
pure function — see [ADR-0011](../../docs/adr/0011-pure-analysis-core.md).

Every failure signature requires at least one **positive** fixture (the
signature must fire) and one **negative** fixture (it must not). A registry test
fails the build if any registered signature has no fixture, so this guarantee
cannot quietly rot as signatures are added.

Fixtures are also the preferred way to turn a misdiagnosis report into a
permanent regression test: the reporter attaches their `run.ndjson`, and it
becomes a fixture here.

Populated in Phase 4.
