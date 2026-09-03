# Failure signature documentation

One page per signature (`SC000` through `SC017`), each covering:

- what was observed, and the rule that fired
- which of the seven termination stages broke
- what it costs in production
- how to fix it, per framework

These pages are embedded into the binary with `go:embed`, so
`shutdowncheck explain SC006` works with no network access. They are also the
project's long-tail documentation: someone searching for "connection reset
during kubernetes rolling update" should land on `SC004`.

Written in Phase 4, published in Phase 8. The catalogue is specified in
section 8 of [shutdowncheck-spec.md](../../shutdowncheck-spec.md).
