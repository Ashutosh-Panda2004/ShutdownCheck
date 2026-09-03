# Conformance suite

This is where "works regardless of your stack" stops being a marketing claim.

The same shutdown defect is implemented in Go, Node and Python. ShutdownCheck is
pointed at each one and must reach the same verdict and raise the same
signatures every time. A broken server that yields a `PASS` anywhere is the one
failure this project cannot tolerate, because a tool that says "you're fine"
when you aren't is worse than no tool at all.

## Layout

```
test/conformance/
  scenarios_test.go            the contract: defects, expected verdicts, signatures
  conformance_runner_test.go   drives the tool against each server (Unix only)
  go/                          reference server
  node/server.js
  python/server.py
```

Each language is a **single file** exposing the same command-line interface:

```
server -addr 127.0.0.1:8080 -mode instant-close
```

One file per language rather than one per defect. The defects differ by a few
lines each, so separate files would have buried the interesting part in
boilerplate and made it easy for the implementations to drift apart.

Every server exposes:

| Endpoint  | Behaviour                                                  |
| --------- | ---------------------------------------------------------- |
| `/work`   | sleeps briefly, then returns `200` — the in-flight request  |
| `/readyz` | `200` while ready, `503` once shutting down                 |

## Modes

| Mode                    | The mistake being reproduced                        | Expected     |
| ----------------------- | --------------------------------------------------- | ------------ |
| `correct`               | none — flips readiness, drains, then exits          | `PASS`       |
| `ignore-signal`         | no handler; only `SIGKILL` stops it                 | SC002        |
| `instant-close`         | closes the moment the signal lands                  | SC006, SC007 |
| `no-readiness-flip`     | drains cleanly but never says it is going away      | SC007        |
| `abrupt-reset`          | tears down connections mid-response                 | SC003        |
| `slow-drain`            | drains far past the grace period                    | SC002        |
| `early-exit`            | exits while requests are still in flight            | not `PASS`   |
| `listener-never-closes` | keeps accepting new work after the signal           | SC005        |
| `slow-readiness`        | flips readiness so late the flip is useless         | SC008        |
| `readiness-flap`        | flaps ready/not-ready instead of latching           | SC016        |
| `nonzero-exit`          | drains correctly, then exits non-zero               | SC013        |
| `accept-no-response`    | accepts connections but never answers them          | SC015        |
| `orphan-child`          | a child inherits the socket and outlives the parent | SC012        |

`correct` is the most important row. Anything can report failures; only a tool
that clears a genuinely correct service is worth putting in CI.

The same defect is deliberately judged under different profiles — `instant-close`
appears twice, failing under `kubernetes` and passing under `strict` — because
whether closing immediately is a bug depends entirely on what is in front of you.

## Two independent stacks, or a documented reason

`TestSignatureCoverageAcrossLanguages` fails the build if a signature is
exercised in only one language. Without it the cross-stack claim decays silently
as scenarios are edited, and "works with your stack" quietly becomes "works in
Go".

SC012 is the one exemption. An orphaned listener is a defect of process
topology — file-descriptor inheritance and process groups — and behaves
identically whatever the service is written in. Reimplementing it per language
would test the fixture's plumbing rather than the tool.

The exemption is itself tested: `TestSingleLanguageExemptionsAreStillNecessary`
fails if a second implementation is ever added and the exemption is left behind
to excuse a gap that no longer exists.

The `orphan-child` server does not re-bind the port. It passes the listening
socket to the child as a file descriptor, which is how the defect actually
occurs: a pre-forked worker shares the parent's listener, so the port stays busy
after the process the orchestrator was watching has exited. Re-binding would
simply fail with "address already in use" and reproduce nothing.

## Running it

```sh
go test ./test/conformance/            # everything available locally
go test -short ./test/conformance/     # contract checks only, no servers
go test -run 'TestConformance/node' ./test/conformance/
```

The servers are driven with real signals, so the runs are Unix-only. The
contract in `scenarios_test.go` carries no build tag and is checked everywhere,
Windows included.

Missing runtimes are skipped locally and are a **hard failure** when `CI` is
set. A silently shrinking matrix must never be able to hide behind a green tick.

## Adding a language

1. Write `<lang>/server.<ext>` implementing the modes above.
2. Register it in `languages()`, listing only the modes it genuinely exhibits.
3. Install the runtime in the `conformance` job in `.github/workflows/ci.yml`.

Claim only what the fixture actually reproduces. Asserting a defect a server
does not really exhibit tests fiction, and the coverage guard cannot tell the
difference.

Java and .NET are the obvious next additions — Tomcat and Kestrel both have
well-known graceful-shutdown sharp edges. They are deliberately left until they
can be verified on a machine with those toolchains installed, rather than
written blind and assumed to work.
