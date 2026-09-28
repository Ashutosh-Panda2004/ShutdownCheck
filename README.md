# ShutdownCheck

> **Status: alpha.** The tool runs end to end and produces a verdict. Process,
> managed-command and Docker targets work, along with every report format, and
> the same shutdown defects are verified in Go, Node.js and Python. On Windows,
> use the Docker target: the platform has no `SIGTERM`, and simulating one
> would produce a verdict about a signal that was never delivered. Kubernetes
> targeting is still to come, and the install methods below describe a release
> that has not been tagged yet.
> Not yet recommended as a merge gate you depend on.

**ShutdownCheck terminates your service the way your orchestrator will — under real load — and tells you exactly which stage of shutdown you got wrong, and how to fix it in your framework.**

## The problem

Every deploy, autoscale-down and node drain does the same thing to your service: `SIGTERM`, wait, `SIGKILL`. If your service mishandles that window, **every deploy silently drops production traffic**. It rarely shows up in testing, because the bug is a race: whether a request dies depends on whether it happened to be mid-flight at the microsecond the signal landed.

Most teams find out from a customer complaint, years later.

## The part almost everyone gets wrong

The usual advice — *"on SIGTERM, immediately stop accepting connections and drain"* — is **wrong under Kubernetes and most load balancers**.

De-registration is asynchronous. Nothing guarantees the routing tables are updated before your process receives the signal, so a service that closes its listener instantly produces a burst of `connection refused`, which your ingress turns into **502s for real users**. The service did graceful shutdown by the book and still dropped traffic.

Correct behaviour is a *lame-duck window*: fail readiness immediately, keep serving for the propagation delay, **then** close the listener and drain. ShutdownCheck models this explicitly via shutdown profiles — it is the difference between a tool that is right and a tool that is merely plausible.

## The seven stages of correct termination

Every check maps to exactly one stage, and every diagnosis names the stage that broke.

| | Stage | Must happen |
|---|---|---|
| S1 | Signal received | Handle `SIGTERM`; don't ignore it |
| S2 | Readiness flipped | Health endpoint starts failing immediately |
| S3 | Lame-duck window | Keep serving new connections while de-registration propagates |
| S4 | Listener closed | Stop accepting once the window elapses |
| S5 | Connection close signalled | `Connection: close` / HTTP-2 `GOAWAY` |
| S6 | In-flight work drained | Every active request completes |
| S7 | Clean exit inside budget | Exit before the grace period expires, no orphans |

The full explanation, including why the usual advice is wrong under Kubernetes,
is in [docs/seven-stages.md](docs/seven-stages.md).

## What it will look like

```console
$ shutdowncheck run --url http://localhost:8080/api/orders -- ./bin/my-server --port 8080
```

ShutdownCheck spawns the process, waits for readiness, calibrates load toward the requested concurrency, verifies that requests were actually in flight when the signal landed, terminates it, and reports. If it cannot prove the in-flight sample, the result is `INCONCLUSIVE`, never a pass:

```
VERDICT: FAIL      score 34/100 (F)      consistent across 3/3 trials

  x SC003  IN_FLIGHT_DROPPED            6 of 22 in-flight requests destroyed
  x SC006  NO_DEREGISTRATION_WINDOW     listener closed 12ms after SIGTERM
  x SC007  READINESS_NOT_FLIPPED        /readyz returned 200 for the entire shutdown
```

Every finding has an explanation and a fix:

```console
$ shutdowncheck explain SC006
```

A run can be recorded and re-judged later without repeating it, which is useful
when deciding whether behaviour that is fine standalone would survive behind a
load balancer. Without an override, replay restores the original effective
policy, gates, metadata and multi-trial summary:

```console
$ shutdowncheck run ... --format ndjson --output run.ndjson
$ shutdowncheck analyze run.ndjson --profile kubernetes
```

Containers are targeted the same way, with the probe URL filled in from the
container's published ports:

```console
$ shutdowncheck run --docker my-api --url /api/orders --readiness-url /readyz
```

Full sample output is in [spec section 3.3](shutdowncheck-spec.md).

## Install

| Method | Command |
| --- | --- |
| Script | `curl -fsSL https://raw.githubusercontent.com/shutdowncheck/shutdowncheck/main/install.sh \| sh` |
| Homebrew | `brew install shutdowncheck/tap/shutdowncheck` |
| Scoop | `scoop bucket add shutdowncheck https://github.com/shutdowncheck/scoop-bucket && scoop install shutdowncheck` |
| Go | `go install github.com/Ashutosh-Panda2004/ShutdownCheck/cmd/shutdowncheck@latest` |
| Docker | `docker run --rm ghcr.io/shutdowncheck/shutdowncheck:latest version` |
| Binaries | [Releases](https://github.com/Ashutosh-Panda2004/ShutdownCheck/releases) — linux, macOS and Windows on amd64 and arm64 |

Then see what a report looks like, with nothing of your own to set up:

```console
$ shutdowncheck demo
```

The demo is a real check against a real process receiving a real `SIGTERM`. No
part of this tool ever prints a report it did not measure.

### Verifying a download

Every release is signed with [cosign](https://docs.sigstore.dev) keyless
signing, ships an SBOM, and carries SLSA build provenance.

```sh
cosign verify-blob checksums.txt \
  --certificate checksums.txt.pem \
  --signature checksums.txt.sig \
  --certificate-identity-regexp 'https://github\.com/shutdowncheck/shutdowncheck/\.github/workflows/.+' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

The install script does the checksum half of this automatically and refuses to
install anything that does not match.

## In CI

```yaml
- uses: shutdowncheck/shutdowncheck@v1.0.0
  with:
    version: v1.0.0
    url: http://localhost:8080/api/orders
    readiness-url: http://localhost:8080/readyz
    command: '["./bin/my-server", "--port", "8080"]'
```

The step fails when a defect is found, writes the report to the job summary, and
uploads the JSON. See [action/](action/) for every input, including reporting
without gating while you work through what it finds.

## Design commitments

- **Never a false PASS.** If the tool cannot prove correct behaviour, it reports `INCONCLUSIVE`. Trust is the only asset a verification tool has.
- **Black box.** No source access, no library import, no agent, no sidecar. The protocol is stack-independent; the current [conformance suite](test/conformance) proves the same defects across Go, Node.js and Python. Java and C# fixtures remain explicit pre-v1.0 follow-up work.
- **Deterministic and explainable.** Every verdict traces to a named rule over recorded evidence. No heuristics, no ML, no AI.
- **Single static binary.** No runtime, no daemon, no cluster install, no account.
- **No telemetry. Ever.**

## Building from source

Requires Go (see `go.mod` for the minimum version).

```console
git clone https://github.com/Ashutosh-Panda2004/ShutdownCheck
cd shutdowncheck
make build          # or: .\make.ps1 build   on Windows
make ci             # lint, test, race, coverage
```

## Documentation

| Document | Purpose |
|---|---|
| [shutdowncheck-spec.md](shutdowncheck-spec.md) | Full technical specification and locked design decisions |
| [docs/adr/](docs/adr/) | Architecture decision records |
| [docs/faq.md](docs/faq.md) | How this differs from Gremlin/Litmus/Chaos Mesh, service meshes, and the business model |
| [docs/demo-script.md](docs/demo-script.md) | 90-second fail → explain → fix → pass demo script with backup recording plan |
| [CONTRIBUTING.md](CONTRIBUTING.md) | How to build, test and propose changes |
| [SECURITY.md](SECURITY.md) | Vulnerability reporting and the tool's security posture |

## Scope

ShutdownCheck does exactly one thing. It is **not** a chaos engineering platform, not an APM, not a load testing tool, and not a service mesh validator. See the Non-Goals section of the spec before proposing features.

## Origin

Built in a weekend by [Ashutosh Panda](https://github.com/Ashutosh-Panda2004) because every "graceful shutdown" tutorial was wrong about Kubernetes. The lame-duck pattern isn't documented in most frameworks, and the only way to learn it was to cause 502s in production. This tool exists so you don't have to.

## License

[Apache-2.0](LICENSE)
