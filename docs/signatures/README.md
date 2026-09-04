# Failure signatures

Every finding ShutdownCheck reports maps to exactly one signature below, and
every signature maps to one of the seven stages of correct termination.

These pages are generated from the catalogue compiled into the binary, so they
cannot drift from what the tool actually reports. Run `go test ./internal/remediate -update` after
changing the catalogue.

The same text is available offline:

```console
$ shutdowncheck explain SC006
```

| ID | Stage | Name | What it means |
| --- | --- | --- | --- |
| [SC000](SC000.md) | — | INSUFFICIENT_INFLIGHT | Too few requests were in flight when the signal landed. |
| [SC001](SC001.md) | S1 | SIGTERM_IGNORED | The process showed no reaction to the signal at all. |
| [SC002](SC002.md) | S7 | SIGKILL_REQUIRED | The process was still alive when the grace period expired. |
| [SC003](SC003.md) | S6 | IN_FLIGHT_DROPPED | Requests that were already being processed failed during shutdown. |
| [SC004](SC004.md) | S5 | ABRUPT_CONNECTION_RESET | Connections were destroyed with RST instead of being closed cleanly. |
| [SC005](SC005.md) | S4 | LISTENER_OPEN_AFTER_WINDOW | The listener kept accepting connections past the de-registration window. |
| [SC006](SC006.md) | S3 | NO_DEREGISTRATION_WINDOW | The listener closed almost immediately after the signal. |
| [SC007](SC007.md) | S2 | READINESS_NOT_FLIPPED | The readiness endpoint stayed healthy for the whole shutdown. |
| [SC008](SC008.md) | S2 | READINESS_FLIP_SLOW | Readiness took too long to start failing. |
| [SC009](SC009.md) | S5 | KEEPALIVE_NOT_TERMINATED | Responses after the signal did not ask clients to close the connection. |
| [SC010](SC010.md) | S7 | SHUTDOWN_BUDGET_EXCEEDED | Shutdown took longer than the declared budget. |
| [SC011](SC011.md) | S6 | EARLY_EXIT | The process exited while requests were still in flight. |
| [SC012](SC012.md) | S7 | PORT_HELD_AFTER_EXIT | The port was still accepting connections after the main process exited. |
| [SC013](SC013.md) | S7 | NONZERO_EXIT_CODE | The process exited with a non-zero status after being asked to stop. |
| [SC014](SC014.md) | S6 | DRAIN_LATENCY_SPIKE | Latency rose sharply while draining. |
| [SC015](SC015.md) | S4 | ACCEPT_WITHOUT_RESPONSE | Connections were accepted after the signal but never answered. |
| [SC016](SC016.md) | S2 | READINESS_FLAPPED | Readiness recovered after starting to fail. |
| [SC017](SC017.md) | S7 | POST_KILL_TRAFFIC_LOSS | Requests were still in flight when SIGKILL landed. |

## The seven stages

| | Stage | Must happen |
| --- | --- | --- |
| S1 | Signal received | Handle `SIGTERM`; don't ignore it |
| S2 | Readiness flipped | Health endpoint starts failing immediately |
| S3 | Lame-duck window | Keep serving new connections while de-registration propagates |
| S4 | Listener closed | Stop accepting once the window elapses |
| S5 | Connection close signalled | `Connection: close` / HTTP-2 `GOAWAY` |
| S6 | In-flight work drained | Every active request completes |
| S7 | Clean exit inside budget | Exit before the grace period expires, no orphans |
