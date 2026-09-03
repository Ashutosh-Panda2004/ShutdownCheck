# Conformance suite

Minimal HTTP servers in Go, Node.js, Python, Java and C#, each in several
deliberately-broken variants plus a correct one.

This directory is three things at once:

1. **The correctness proof.** The end-to-end harness runs ShutdownCheck against
   every variant and asserts the exact expected signature set. Every signature
   must be reproducibly triggered by fixtures in at least two languages, and no
   broken variant may ever yield `PASS`.
2. **The demo material** used in the README and documentation.
3. **Public reference material** for "what correct termination looks like in my
   language" — which is genuinely hard to find and is a large part of why this
   project exists.

Planned variants per language:

| Variant | Behaviour | Expected signatures |
|---|---|---|
| `correct` | Implements all seven stages | none |
| `ignores-signal` | No signal handler at all | `SC001`, `SC002` |
| `instant-close` | Closes the listener immediately on signal | `SC006` |
| `no-readiness-flip` | Keeps returning healthy until exit | `SC007` |
| `abrupt-reset` | Destroys sockets instead of draining | `SC003`, `SC004` |
| `slow-drain` | Drains, but overruns the grace period | `SC002`, `SC010` |
| `early-exit` | Exits while requests are still in flight | `SC011` |
| `orphan-child` | Parent exits, child keeps the port bound | `SC012` |

Servers are intentionally minimal and heavily commented. They are excluded from
linting and from the architecture import graph, because they are meant to
demonstrate anti-patterns.

Built in Phase 6. See [DEVELOPMENT-PLAN.md](../../DEVELOPMENT-PLAN.md).
