## ShutdownCheck: FAIL

**Score 46/100 (F)** &middot; profile `kubernetes` &middot; target `./bin/api --port 8080`

| Phase | Requests | OK | Failed |
|---|---:|---:|---:|
| Before the signal | 40 | 40 | 0 |
| In flight at signal | 22 | 16 | 6 |
| After the signal | 24 | 0 | 24 |

### Findings

| | ID | Finding | Detail |
|---|---|---|---|
| **error** | [`SC003`](https://shutdowncheck.dev/signatures/SC003) | IN_FLIGHT_DROPPED | 6 of 22 in-flight request(s) were destroyed during shutdown. |
| **error** | [`SC006`](https://shutdowncheck.dev/signatures/SC006) | NO_DEREGISTRATION_WINDOW | The listener closed 10ms after the signal, before de-registration could propagate. |
| **error** | [`SC007`](https://shutdowncheck.dev/signatures/SC007) | READINESS_NOT_FLIPPED | The readiness endpoint kept reporting healthy for the entire shutdown. |
| warn | [`SC009`](https://shutdowncheck.dev/signatures/SC009) | KEEPALIVE_NOT_TERMINATED | 1 connection(s) were reused after the signal, but the server never asked clients to close them. |
| warn | [`SC014`](https://shutdowncheck.dev/signatures/SC014) | DRAIN_LATENCY_SPIKE | Latency during drain rose to 3.7x the baseline (p99 220ms against 60ms). |
