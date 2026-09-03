# ShutdownCheck — Technical Specification & Project Brief (v0.2 — Locked for Implementation)

> **Status:** v0.2 supersedes the v0.1 exploratory draft. All six v0.1 open questions are now **decided** (Section 4). The scope has been deliberately *expanded in depth, not in breadth*: the tool still does exactly one thing — verify termination behaviour — but it now does it with orchestrator-faithful semantics, connection-layer forensics, and a deterministic diagnosis engine rather than a bare pass/fail counter.
>
> **Audience:** implementers (human or AI), contributors, and reviewers. Everything here is intended to be buildable as written. Sections still marked **[OPEN]** are non-blocking and can be resolved during the phase they belong to.

---

## 1. Problem Statement

### 1.1 The core problem

Every backend service that runs inside an orchestrator (Kubernetes, ECS, Nomad, Docker Swarm, systemd-managed VMs) is subject to the same lifecycle event during every deploy, autoscale-down, node drain, spot-instance reclaim, and HPA scale-in:

1. The orchestrator decides to stop the process.
2. It sends `SIGTERM`.
3. It waits a configured grace period (Kubernetes defaults to 30s; `docker stop` defaults to 10s).
4. If the process hasn't exited by then, it sends `SIGKILL`.

What happens in that window determines whether real user requests succeed or fail. If the service:

- keeps accepting new connections *too long* after `SIGTERM`,
- stops accepting new connections *too early*, before the load balancer has de-registered it,
- doesn't finish in-flight requests before exiting,
- doesn't signal connection close (`Connection: close` / HTTP/2 `GOAWAY`), leaving clients reusing a dying keep-alive socket,
- or simply ignores `SIGTERM` entirely and gets hard-killed,

then **every single deploy silently drops a percentage of production traffic.** This is one of the most common and most invisible reliability bugs in backend systems. It doesn't show up in normal testing because:

- Local dev environments rarely simulate `SIGTERM` under real concurrent load.
- Unit and integration tests almost never exercise process lifecycle *while traffic is in flight*.
- The failure is often small (a few dropped requests per deploy) and gets attributed to "normal noise" rather than root-caused.
- It compounds: a team deploying 10x/day loses a small amount of traffic 10 separate times, invisibly, forever — until it's investigated specifically.
- It is **non-deterministic by nature**: whether a request is dropped depends on whether it happened to be mid-flight at the microsecond the signal landed. This is precisely why it needs a purpose-built tool rather than a manual check.

### 1.2 The part almost everyone gets wrong

The naive mental model — *"on SIGTERM, immediately stop accepting connections and drain"* — is **wrong in every orchestrator that uses a load balancer or service mesh**, which is to say, nearly all of them.

The reason is that de-registration is *eventually consistent* and *concurrent* with signal delivery:

```
kubelet                     SIGTERM ──────────────▶ your process
endpoints controller        pod removed from Endpoints
kube-proxy / CNI / mesh     iptables & routing tables updated ...eventually
external LB / ingress       target group drain begins ..........eventually
```

Nothing guarantees the routing update lands *before* your process gets the signal. In practice it frequently lands **hundreds of milliseconds to several seconds after**. So a service that closes its listener the instant `SIGTERM` arrives will produce a burst of `connection refused` → the ingress turns that into **502s for real users**, on every single deploy. The service did "graceful shutdown" by the book and still dropped traffic.

The *correct* behaviour is a **lame-duck window**: fail readiness immediately, but keep serving new connections for a configured period, and only then close the listener and drain.

This nuance is the single most important thing a shutdown-verification tool must model — and it is exactly what a naive "did any request fail?" checker gets backwards. **ShutdownCheck models it explicitly** (Section 7.6, shutdown profiles).

### 1.3 The seven stages of a correct termination

This is the conceptual backbone of the entire tool. Every check ShutdownCheck performs maps to exactly one stage, and every diagnosis names the stage that broke.

| # | Stage | What must happen | Common failure |
|---|---|---|---|
| **S1** | **Signal received** | Process handles `SIGTERM` (does not ignore or die instantly) | Signal ignored → `SIGKILL` at grace expiry |
| **S2** | **Readiness flipped** | Health/readiness endpoint starts failing *immediately* so the orchestrator de-registers the instance | Readiness keeps returning `200` for the whole shutdown → traffic keeps being routed |
| **S3** | **Lame-duck window** | Continue accepting **and successfully serving** new connections for the de-registration propagation delay | Listener closed instantly → `connection refused` → 502s at the ingress |
| **S4** | **Listener closed** | Stop accepting *new* connections once the window elapses; keep existing ones alive | Listener never closes → process hangs until `SIGKILL` |
| **S5** | **Connection close signalled** | `Connection: close` on HTTP/1.1 responses, `GOAWAY` on HTTP/2, so clients stop reusing the socket | Keep-alive sockets left "open" then abruptly `RST` → client-side errors |
| **S6** | **In-flight work drained** | Every request already being processed completes successfully; background work (queue consumers, outbox flush, telemetry export) also finishes | Requests truncated mid-response, sockets `RST` |
| **S7** | **Clean exit inside budget** | Process exits (ideally code `0`) comfortably before the grace period expires; no orphaned children still holding the port | Exit overruns grace → `SIGKILL` → everything still in flight is destroyed |

A tool that only measures "requests failed: yes/no" can tell you *something broke*. A tool that measures all seven stages can tell you **which one**, and therefore what to change. That is the product.

### 1.4 Why existing tooling doesn't solve this

| Category | Examples | Why it doesn't fully solve this |
|---|---|---|
| APM / observability platforms | Datadog, New Relic, Honeycomb | Show *that* errors spiked during deploys, but don't isolate, reproduce, or explain shutdown-specific behaviour. Requires the tool to already be integrated *and* the bug to already be hurting production. Detection, not prevention. |
| Chaos engineering platforms | Gremlin, Litmus, Chaos Mesh | Can inject process kills as *one* feature among dozens. Cluster-scoped, heavyweight, requires an installed control plane. Not usable as a 20-second pre-merge gate, and they tell you *it broke*, not *which shutdown stage broke*. |
| Load testers | k6, Vegeta, `hey`, Locust, Gatling | Generate traffic well, but have no concept of a termination signal, no phase correlation against `T_sigterm`, no process-exit monitoring, and no shutdown-specific verdict. You can hand-roll a script — and get flaky, uninterpretable results. |
| Per-language tutorials / blog posts | "Graceful shutdown in Node/Python/Go" guides | Explain how to *write* the handler; provide nothing to *verify* it works; are language-specific; and a large fraction of them teach the S3 anti-pattern (close listener immediately). |
| Framework shutdown hooks | Go `http.Server.Shutdown()`, Node `server.close()`, ASP.NET Core `IHostApplicationLifetime`, Spring `ContextClosedEvent` | These are building blocks *inside* your app, not verification. They also silently do the wrong thing by default (e.g. `server.close()` in Node does not close idle keep-alive sockets on older versions). |
| Kubernetes conformance / policy tools | `kube-score`, Polaris, Datree, OPA/Kyverno | Lint your *YAML* (does the pod have a `preStop` hook? a grace period?). They cannot observe whether the **running binary** actually honours it. Static config check vs. dynamic behavioural proof. |

**The gap:** there is no standalone, language-agnostic, black-box tool whose entire job is: *point me at a service, I will terminate it exactly the way your orchestrator will, under real load, and tell you which of the seven shutdown stages you got wrong and how to fix it in your stack.*

### 1.5 Why this matters (impact framing)

- **Universal.** Applies to any long-running server process, regardless of language, framework, or orchestrator. The addressable surface is "every backend service that has ever been deployed twice."
- **Continuous.** Not a one-time migration issue — it recurs on every deploy, forever, for the life of the service.
- **Asymmetric.** Cheap to fix once found (usually <30 lines), expensive to find without dedicated tooling. That asymmetry is exactly where a focused tool creates outsized value.
- **CI-shaped.** It fits naturally as a merge gate ("don't ship this build if shutdown behaviour regresses"), which is a proven adoption pattern for developer tools (linters, contract testing, `govulncheck`).
- **Teaching-shaped.** Because the correct behaviour is genuinely subtle (Section 1.2), a tool that *explains* the failure becomes the canonical reference for how termination should work — which is how developer tools earn durable mindshare.

---

## 2. Positioning & Differentiation

### 2.1 One-line description

**ShutdownCheck is a black-box CLI that terminates your service the way your orchestrator will — under real load — and tells you exactly which stage of shutdown you got wrong, and how to fix it in your framework.**

### 2.2 The three things that make it defensible

Anyone can write "load generator + `kill -TERM` + count failures" in an afternoon. That is a *script*, not a product, and it will produce flaky, uninterpretable results. The durable value sits in three places:

1. **Orchestrator-faithful simulation.** Not just "send SIGTERM." Model `preStop` hooks, de-registration propagation delay, the lame-duck window, the real grace-period budget, and the actual `SIGKILL` escalation at expiry. The run should be a *rehearsal of the deploy*, not an approximation of it.
2. **A deterministic diagnosis engine.** A catalogue of named, documented failure signatures (Section 8) with stack-specific remediation. Output is *"`SC006 NO_DEREGISTRATION_WINDOW` — your listener closed 12ms after SIGTERM; in Kubernetes this causes 502s; here is the fix for ASP.NET Core"* — not *"3 requests failed."*
3. **Statistical honesty.** The bug being hunted is inherently a race. A tool that says PASS when it simply never had any requests in flight at the moment of the signal is *worse than useless* — it manufactures false confidence. ShutdownCheck auto-calibrates load to guarantee a meaningful in-flight sample and reports **INCONCLUSIVE** rather than PASS when it cannot (Sections 7.5, 9).

Those three together are hard to replicate casually, are genuinely useful, and are what turn this from a weekend script into infrastructure people depend on.

### 2.3 Why black-box design is the key differentiator

The tool works purely through:

- Real requests over the network, with connection-level instrumentation (HTTP/1.1 in v1; HTTP/2 and gRPC on the roadmap).
- A real termination signal to a real process/container/pod.
- External observation of responses, sockets, listener state, and process exit.

**It never touches your source code, never requires a library import, never requires instrumentation, never requires a sidecar.** A Go service, a Java service, a C# service and a Python service are all tested through the identical CLI invocation. That is what makes "works regardless of tech stack" a literal, testable claim rather than a slogan — and it is why the tool can be adopted in 60 seconds by a team that has never heard of it.

---

## 3. Product Concept & UX

### 3.1 Three tiers of interaction

The single biggest adoption lever is that the *first* run must require nearly zero thought. Tiering the UX achieves that without limiting power users.

**Tier 1 — Zero-config (the 60-second first experience).** ShutdownCheck launches the service itself, waits for the port to become ready, auto-calibrates load, terminates it, and reports. No PID juggling, no backgrounding, no `sleep 5` in CI:

```console
$ shutdowncheck run --url http://localhost:8080/api/orders -- ./bin/my-server --port 8080
```

**Tier 2 — Attach to something already running.** For local debugging against a dev server you started yourself:

```console
$ shutdowncheck run --url http://localhost:8080/api/orders --pid 12345
```

**Tier 3 — Orchestrated targets.** Progressively closer to production reality:

```console
$ shutdowncheck run --url http://localhost:8080/health --docker my-api
$ shutdowncheck run --url https://svc.internal/health --k8s-pod prod/my-api-7c9f-x2k1 --profile kubernetes
```

**CI usage** collapses to one line, because the config lives in the repo:

```console
$ shutdowncheck run --config shutdowncheck.yaml --scenario api --format junit --output results.xml
```

### 3.2 What actually happens during a run

1. **Preflight.** Validate flags, resolve the target, confirm it is reachable and signalable, confirm the readiness endpoint responds, refuse obviously dangerous targets (Section 14).
2. **Warmup.** Low-rate traffic to prime connection pools, JIT, and caches. Warmup requests are recorded but excluded from scoring.
3. **Calibration.** Measure baseline latency and compute the request rate needed to guarantee the configured in-flight concurrency at the moment of termination (Section 7.5).
4. **Steady state.** Sustained, rate-controlled load with connection-level tracing on every request.
5. **`preStop` simulation** (optional). Hold traffic steady for the configured pre-stop delay, exactly as Kubernetes would before delivering the signal.
6. **Termination.** Send `SIGTERM` with nanosecond-precision timestamping on the same monotonic clock as every request event.
7. **Post-signal traffic.** Keep generating load across the whole grace-period budget — because in a real deploy, traffic *keeps arriving* at a terminating instance.
8. **Observation.** Concurrently poll readiness, poll listener acceptance, tail target logs, and watch for process exit.
9. **`SIGKILL` escalation.** At grace expiry, actually deliver `SIGKILL` — faithfully reproducing what the orchestrator would do — and record everything destroyed by it.
10. **Analysis.** Correlate one unified timeline → classify every request and connection → evaluate signatures → compute verdict and score.
11. **Report.** Render human/JSON/JUnit/Markdown output, set the exit code, optionally emit a badge.

### 3.3 Example terminal output (illustrative — this is the target UX)

```
ShutdownCheck v1.0.0   profile=kubernetes   target=process(pid 12345)   trial 1/3

  http://localhost:8080/api/orders   POST   calibrated to 340 rps (22 in-flight)

TIMELINE                                        T=0 is SIGTERM
        -3s        -2s        -1s         0s        +2s        +4s        +6s
         |----------|----------|----------|----------|----------|----------|
 traffic ████████████████████████████████████░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░
                                            ^ 96 requests refused from +0.01s
 ready   200 200 200 200 200 200 200 200 200 200 200 200 200 200 200 200 200
                                            ! readiness never flipped
 listen  OPEN ───────────────────────────────╳ closed at +0.012s
 proc    RUNNING ──────────────────────────────────────────────╳ exit +6.4s (code 0)
 budget  [═════════════ grace period 5.0s ═════════════]▓ SIGKILL sent at +5.0s

REQUESTS
  before SIGTERM          1,024      1,024 ok        0 failed
  in-flight at SIGTERM       22         16 ok        6 FAILED   (4 reset, 2 timeout)
  after SIGTERM             318          0 ok      318 refused

CONNECTIONS
  reused keep-alive sockets after SIGTERM        41
  connections closed with `Connection: close`     0     (expected: all)
  connections terminated by RST (not FIN)        41     ← abrupt

VERDICT: FAIL      score 34/100 (F)      consistent across 3/3 trials

  ✗ SC003  IN_FLIGHT_DROPPED            6 of 22 in-flight requests destroyed
  ✗ SC006  NO_DEREGISTRATION_WINDOW     listener closed 12ms after SIGTERM
  ✗ SC007  READINESS_NOT_FLIPPED        /readyz returned 200 for the entire shutdown
  ✗ SC009  KEEPALIVE_NOT_TERMINATED     41 sockets RST without Connection: close
  ✗ SC002  SIGKILL_REQUIRED             process had not exited at grace expiry

  ! Detected stack: ASP.NET Core (Kestrel)          run `shutdowncheck explain SC006`

  SC006 is the highest-impact finding. Your service stops accepting connections
  immediately on SIGTERM, but Kubernetes removes the pod from Endpoints
  asynchronously — so the ingress keeps routing to you for a short window and
  every one of those requests becomes a 502 for a real user.

  Fix (ASP.NET Core): add a de-registration delay before the host stops accepting.
      builder.Services.Configure<HostOptions>(o =>
          o.ShutdownTimeout = TimeSpan.FromSeconds(30));
      // + a preStop hook of `sleep 5` in your pod spec
  Full remediation: https://shutdowncheck.dev/signatures/SC006

exit code 1
```

The timeline block is deliberately the visual centrepiece: it is what people screenshot and share, and it communicates the entire seven-stage model at a glance.

### 3.4 Design principles (binding on all implementation decisions)

1. **Never a false PASS.** If the tool cannot prove correct behaviour, it reports `INCONCLUSIVE`, never `PASS`. Trust is the only asset.
2. **Deterministic and explainable.** Every verdict traces to a named rule over recorded evidence. No heuristics, no scoring models, no AI.
3. **Explain, don't just fail.** Every failure names the broken stage, the production consequence, and a stack-specific fix.
4. **Zero-config first, fully configurable second.** Sensible defaults must produce a meaningful result on the first try.
5. **CI-native.** Deterministic exit codes, machine formats, quiet mode, no TTY assumptions, no network egress beyond the target.
6. **Fast.** A default run is ~20–30s. A CI gate that takes minutes gets deleted.
7. **Single static binary.** No runtime, no daemon, no cluster install, no account, no telemetry.


---

## 4. Locked Decisions (resolves all v0.1 open questions)

These are decided. Changing any of them requires a written ADR in `docs/adr/`.

| # | Question | **Decision** | Rationale |
|---|---|---|---|
| D1 | Implementation language: Go or C#? | **Go** | Single static binary, trivial cross-compilation to 6 platforms, no runtime on the host. A black-box tool that requires *its own* runtime contradicts its entire premise. Go also gives first-class `syscall`/`os/exec` signal handling, `httptrace` connection instrumentation (essential for Section 7.7), and matches ecosystem expectations for infra CLIs. The C#/.NET background stays engaged via the conformance suite and the ASP.NET Core remediation rules. |
| D2 | Container targeting in MVP? | **Yes — Docker in v1.0, but behind a `Target` interface from day one** | The interface is the real design decision; once it exists, `DockerTarget` is ~150 lines (`docker kill --signal=TERM`, `docker inspect` polling, `docker logs` capture). Shipping v1.0 with process + managed-command + Docker covers the overwhelming majority of local and CI usage. Kubernetes lands in v1.1. |
| D3 | GET-only load generation? | **No — full request spec in v1.0** | Method, headers, body, and body-from-file. Cost is small (the load generator is not the hard part); the realism gain is large, since drain bugs concentrate on slow write paths, not `/health`. Weighted multi-endpoint scenarios are also supported via config file. |
| D4 | Default `--rps` / `--duration`? | **No fixed default — auto-calibrate to a target in-flight count** | The user should never have to reason about rps. Default goal is `--ensure-in-flight 20`; the tool measures baseline latency during warmup and derives the rate (Section 7.5). If the goal is unreachable, the run is `INCONCLUSIVE`, not `PASS`. This is the single most important correctness decision in the tool. |
| D5 | Tolerance window for post-signal acceptance? | **Neither strict nor a fudge factor — an explicit shutdown profile** | "Accepting after SIGTERM" is correct under `kubernetes`/`lame-duck` profiles and incorrect under `strict`. The tool ships three profiles with an `--accept-window`, auto-selected by target type (Section 7.6). This is a differentiator, not a compromise. |
| D6 | Naming | **Keep `ShutdownCheck`; binary `shutdowncheck`, alias `sdc`** | Literal problem-name discoverability is a real SEO advantage for a niche infra tool. Verify collisions on GitHub / Homebrew / npm / crates.io / PyPI and register the org + `shutdowncheck.dev` before the first public commit. Fallback shortlist: `drainkit`, `lameduck`, `terminus-check`. |
| D7 | Should the tool actually send `SIGKILL` at grace expiry? | **Yes, `--enforce-sigkill` default `true`** | Only faithful way to measure what a real deploy destroys. Also guarantees runs terminate and never leak processes in CI. |
| D8 | Windows support | **Docker target works on Windows in v1.0; native Windows process targets in v1.2** | Windows has no `SIGTERM`; `CTRL_BREAK_EVENT` + job objects is a different model deserving its own design. The signal abstraction is designed so this is additive, not a rewrite. |
| D9 | Report schema stability | **`pkg/schema` is a public, versioned Go package with `schema_version`** | Third parties will build on the JSON. Breaking it silently is how a tool loses integrations. |
| D10 | Telemetry | **None. Ever.** | An infra tool that phones home is an infra tool that gets banned by security review. |

---

## 5. Explicit Non-Goals (out of scope for all versions unless stated)

To keep the tool sharply scoped and prevent feature creep:

- **No AI/ML.** No LLM analysis, no learned models, no "smart" suggestions. Diagnosis is a deterministic rule engine over recorded evidence, and must stay auditable. This is a deliberate product position, not a limitation.
- **Not a chaos engineering platform.** No network partitioning, no CPU/memory stress, no dependency fault injection, no cluster-wide experiments. Exactly one failure domain: process termination.
- **Not an APM.** No agents, no persistent dashboards, no long-term storage, no alerting. Point-in-time verification, on demand or in CI.
- **Not a load-testing tool.** Load exists solely to guarantee in-flight requests at the moment of termination. No intention to compete with k6/Gatling/Locust on throughput, scripting, or protocol breadth.
- **Not a service mesh / LB validator.** The tool verifies *your process*. It does not test whether your ingress controller or mesh is configured correctly (though it will tell you when your process is incompatible with how they behave).
- **No hosted SaaS, no accounts, no license keys, no open-core paywall in v1.** The tool is complete and useful standing alone; anything else is a separate future conversation.
- **No mandatory in-process library.** An optional companion library may exist someday, but the black-box tool must never depend on it.
- **No PLC / industrial systems angle.** Purely generic backend HTTP/gRPC services.

---

## 6. v1.0 Scope — The Actual First Release

Everything below ships in v1.0. Anything not listed is deferred (Section 18).

### 6.1 Targets

| Target | Flag | Notes |
|---|---|---|
| Managed process | `-- <cmd> [args...]` | ShutdownCheck spawns it, waits for readiness, owns the process group, guarantees cleanup. **The flagship UX.** |
| Existing process | `--pid <n>` | Attach by PID. |
| Docker container | `--docker <name\|id>` | Signal via `docker kill --signal`, liveness via `docker inspect`, logs via `docker logs --follow`. |

All three sit behind one `Target` interface (Section 7.3). Adding Kubernetes later touches no other package.

### 6.2 Load generation & probing

- Rate-controlled generator with bounded worker pool and back-pressure; open-model scheduling (fixed arrival rate, not closed-loop) so a stalling server cannot silently reduce offered load.
- **Auto-calibration** to a target in-flight concurrency (`--ensure-in-flight`, default 20); explicit `--rps` overrides it.
- Full request spec: method, path, headers, body / `--body-file`.
- Optional **weighted multi-request scenarios** via config file (e.g. 80% fast read, 20% slow write) — the realistic case, and cheap to implement once the request spec exists.
- Optional `--slow-url` used specifically to guarantee long-lived in-flight requests when the primary endpoint is too fast to ever be caught mid-flight.
- Keep-alive on by default, `--keep-alive=false` to force fresh connections (they exercise different server code paths and different bugs).
- Configurable per-request timeout; TLS with verification on by default.

### 6.3 Termination realism

- Configurable signal (`TERM` default; `INT`, `QUIT` supported).
- Optional `--prestop-sleep` to simulate a Kubernetes `preStop` hook.
- Signal timestamped on the same monotonic clock as every request/connection event.
- Real `SIGKILL` escalation at grace expiry (`--enforce-sigkill`, default on), with everything it destroys recorded.
- Grace-period default follows the target: 30s for process/Kubernetes semantics, 10s for Docker.

### 6.4 Observation

- **Per-request records:** send/complete timestamps, status, latency, outcome class (`ok`, `refused`, `reset`, `timeout`, `eof`, `tls_error`, `http_error`, `dns_error`), and phase.
- **Per-connection records** via `httptrace`: whether the connection was new or reused, when it was established, whether the server sent `Connection: close`, and whether it ended in `FIN` or `RST`.
- **Listener acceptance probe:** an independent low-frequency TCP dial loop that determines precisely when the socket stopped accepting — independent of whether HTTP requests succeed.
- **Readiness probe loop:** polls `--readiness-url` throughout the run, recording exactly when (or whether) readiness flipped relative to the signal.
- **Process exit monitoring:** exit detection, exit code / termination signal, time-to-exit, and post-exit port-still-bound detection (catches orphaned children).
- **Target log capture** for managed-command and Docker targets, interleaved into the timeline.

### 6.5 Analysis & verdict

- Unified monotonic timeline correlating every event.
- Phase classification per request and per connection (Section 7.6).
- **Shutdown profiles**: `strict`, `lame-duck`, `kubernetes` — auto-selected by target, overridable.
- **Diagnosis engine** with the named signature catalogue (Section 8) and stack-specific remediation.
- **Verdict**: `PASS` / `FAIL` / `INCONCLUSIVE`, plus a 0–100 **Shutdown Score** with a letter grade (Section 9).
- **Multi-trial runs** (`--trials`, default 1; recommended 3 in CI) with consistency reporting, so a flaky result is visible as flaky rather than silently coin-flipped.

### 6.6 Output & CI integration

- Human terminal report with the timeline visualisation (colour, auto-disabled when not a TTY or `NO_COLOR` is set).
- `--format json` (versioned schema), `junit` (native CI test reporting), `markdown` (GitHub Actions job summary / PR comment), `ndjson` (raw event stream for custom analysis).
- `--output <path>` alongside stdout; `--badge <path>` for an SVG score badge.
- Deterministic exit-code taxonomy (Section 9.3), never a bare `1` for everything.
- `shutdowncheck.yaml` config file with named scenarios and gate thresholds.
- `shutdowncheck explain <SIGNATURE>` subcommand — offline documentation of every failure mode.
- **A published GitHub Action** (pulled forward from v1.3 in the v0.1 draft — it is the primary distribution channel and is cheap once the binary and JUnit output exist).

### 6.7 Conformance suite (ships with the repo, not a separate project)

A `test/conformance/` tree containing minimal HTTP servers in **Go, Node.js, Python, Java, and C#**, each in several deliberately-broken variants (`ignores-signal`, `instant-close`, `no-readiness-flip`, `abrupt-reset`, `slow-drain`) plus a `correct` variant. This is simultaneously:

- the tool's own end-to-end test corpus (every signature must be reproducibly triggered by at least one fixture),
- the demo material for the README,
- and a genuinely useful public reference for "what correct termination looks like in my language."

### 6.8 Documentation

README with an animated demo, install instructions, quick start, full CLI reference, config reference, JSON schema reference, one documentation page per failure signature, CI integration guides (GitHub Actions, GitLab CI), and a "seven stages" conceptual explainer.

### 6.9 Explicitly deferred past v1.0

- Kubernetes pod targeting (v1.1), Docker Compose service targeting (v1.1)
- HTTP/2 + `GOAWAY` verification (v1.2), gRPC (v1.3)
- Native Windows process targets (v1.2)
- Historical run storage / regression comparison (v2.0)
- Rolling-deploy simulation with an embedded load balancer (v2.x)
- HTML reports, hosted service, plugin system (unscheduled)

---

## 7. Technical Design

### 7.1 High-level architecture

```
                         shutdowncheck run
                                 │
                    ┌────────────▼────────────┐
                    │      Run Orchestrator    │  state machine:
                    │  (internal/run)          │  preflight → warmup → calibrate
                    └────────────┬────────────┘  → steady → prestop → signal
                                 │                → observe → kill → analyse
   ┌──────────────┬──────────────┼──────────────┬──────────────┐
   │              │              │              │              │
┌──▼──────────┐ ┌─▼───────────┐ ┌▼────────────┐ ┌▼───────────┐ ┌▼──────────┐
│ Load Engine │ │ Readiness   │ │  Listener   │ │   Target   │ │  Target   │
│ open-model  │ │ Prober      │ │  Prober     │ │  (signal / │ │  Log      │
│ scheduler + │ │ polls       │ │  raw TCP    │ │  liveness /│ │  Tailer   │
│ HTTP prober │ │ /readyz     │ │  dial loop  │ │  exit code)│ │           │
│ + httptrace │ │             │ │             │ │            │ │           │
└──────┬──────┘ └──────┬──────┘ └──────┬──────┘ └─────┬──────┘ └─────┬─────┘
       │               │               │              │              │
       └───────────────┴───────┬───────┴──────────────┴──────────────┘
                               │  all events carry a monotonic offset
                    ┌──────────▼───────────┐
                    │   Timeline Recorder   │  concurrent-safe, bounded,
                    │  (internal/timeline)  │  single source of truth
                    └──────────┬───────────┘
                               │
                    ┌──────────▼───────────┐
                    │   Analysis Engine     │  classify → detect signatures
                    │  (internal/analyze)   │  → verdict → score
                    └──────────┬───────────┘
                               │
       ┌───────────────┬───────┴───────┬───────────────┬──────────────┐
  ┌────▼─────┐   ┌─────▼────┐    ┌─────▼────┐    ┌─────▼────┐   ┌─────▼────┐
  │  human   │   │   json   │    │  junit   │    │ markdown │   │  badge   │
  │ +timeline│   │ (schema) │    │   xml    │    │          │   │   svg    │
  └──────────┘   └──────────┘    └──────────┘    └──────────┘   └──────────┘
                               │
                        exit code taxonomy

   Targets (pluggable):  Process(pid) │ Command(spawned) │ Docker │ [K8s v1.1]
   Everything is black-box: no agent, no import, no source access.
```

Key structural decisions:

- **Four independent observers.** HTTP load, readiness, raw listener acceptance, and process liveness are measured by *separate* concurrent probes. This matters: a server can accept TCP but never respond (`SC015`), or exit while the port stays bound by an orphaned child (`SC012`). Neither is detectable if you only look at HTTP responses.
- **One timeline, one clock.** Every subsystem writes into a single recorder using a monotonic offset from run start. All correlation happens after the fact on immutable data, which makes analysis pure, unit-testable, and replayable.
- **Analysis is a pure function.** `Analyze(Timeline, Policy) → Report`. No I/O, no clock reads. Every signature can be tested against a synthetic timeline fixture with zero network or process involvement — which is what makes the diagnosis engine trustworthy.


### 7.2 Repository & package layout

```
shutdowncheck/
├── cmd/shutdowncheck/          # main(); cobra command wiring only
├── internal/
│   ├── cli/                    # flag definitions, config merge, subcommands
│   ├── config/                 # YAML config model, defaults, validation
│   ├── run/                    # run orchestrator state machine
│   ├── load/                   # open-model scheduler, worker pool, calibration
│   ├── probe/                  # HTTP prober (+httptrace), readiness, TCP listener probe
│   ├── target/                 # Target interface: process, command, docker, (k8s)
│   ├── timeline/               # event types, recorder, monotonic clock abstraction
│   ├── analyze/                # classification, signature rules, verdict, scoring
│   ├── remediate/              # stack fingerprinting + fix-hint catalogue
│   └── report/                 # human/timeline, json, junit, markdown, badge renderers
├── pkg/schema/                 # PUBLIC versioned report types (importable by 3rd parties)
├── test/
│   ├── conformance/            # broken/correct servers: go, node, python, java, dotnet
│   ├── fixtures/               # synthetic timelines for pure analyzer tests
│   └── e2e/                    # end-to-end harness driving conformance servers
├── action/                     # GitHub Action wrapper (action.yml + entrypoint)
├── docs/
│   ├── adr/                    # architecture decision records
│   └── signatures/             # one page per SCxxx failure signature
└── .github/workflows/
```

Rules: `internal/analyze` and `pkg/schema` must have **no I/O imports** — this is enforced by an import-lint test. `cmd/` contains no logic.

### 7.3 Core interfaces

```go
// target.Target abstracts "a thing that can be terminated and observed".
// One interface is what makes Docker/K8s additive rather than invasive.
type Target interface {
    Describe() Descriptor                            // kind, id, human label
    Start(ctx context.Context) error                 // no-op for attach-style targets
    WaitReady(ctx context.Context, r ReadyCheck) error
    Signal(ctx context.Context, sig Signal) error    // TERM/INT/QUIT/KILL
    Wait(ctx context.Context) (ExitStatus, error)    // blocks until exit
    Alive(ctx context.Context) (bool, error)
    Logs(ctx context.Context) (io.ReadCloser, bool)  // ok=false if unsupported
    DefaultGracePeriod() time.Duration               // 30s process/k8s, 10s docker
    Close() error                                    // guaranteed cleanup
}

// timeline.Recorder is the single sink for every observation.
type Recorder interface {
    Record(Event)                  // safe for concurrent use, non-blocking
    Snapshot() Timeline            // immutable copy for analysis
}

// analyze is pure: no clock, no network, no filesystem.
type Analyzer interface {
    Analyze(t timeline.Timeline, p Policy) schema.Report
}

// Each failure mode is an independent, individually testable rule.
type Signature interface {
    ID() string                    // "SC006"
    Stage() Stage                  // S1..S7
    Evaluate(timeline.Timeline, Policy) (Finding, bool)
}
```

### 7.4 Run lifecycle state machine

```
PREFLIGHT ─▶ START_TARGET ─▶ WAIT_READY ─▶ WARMUP ─▶ CALIBRATE ─▶ STEADY
                                                                     │
                                        ┌────────────────────────────┘
                                        ▼
                                  PRESTOP_HOLD (optional)
                                        │
                                        ▼
                                  SIGNAL  ── T=0, monotonic ──┐
                                        │                      │
                                        ▼                      ▼
                                  OBSERVE (load continues) ── EXIT_WATCH
                                        │                      │
                          grace expires │                      │ exited
                                        ▼                      │
                                  SIGKILL ─────────────────────┤
                                        │                      │
                                        ▼                      ▼
                                  POST_EXIT_CHECK (port still bound?)
                                        │
                                        ▼
                                     ANALYSE ─▶ REPORT ─▶ EXIT_CODE
```

Every transition is recorded as a timeline event, so the report can always explain what the tool itself was doing at any moment. Cleanup (`Target.Close()`, kill process group, remove temp files) is unconditional via `defer`, including on panic or `SIGINT` of the tool itself.

### 7.5 Auto-calibration — guaranteeing a valid experiment

This is the algorithm that makes results trustworthy, and it is what most hand-rolled scripts get wrong.

To catch a drain bug you must have requests *actually in flight* at the instant the signal lands. By Little's Law:

$$L = \lambda W$$

where $L$ is concurrent in-flight requests, $\lambda$ is arrival rate, and $W$ is mean response time. So to hold $L_{target}$ requests in flight:

$$\lambda = \frac{L_{target}}{W_{baseline}}$$

Algorithm:

1. **Warmup** at a low fixed rate for `--warmup` (default 3s). Discard the first 20% of samples (cold start / JIT).
2. Compute $W_{baseline}$ = median response time (median, not mean — robust against warmup outliers).
3. $\lambda = \lceil L_{target} / W_{baseline} \rceil$, clamped to `--max-rps` (default 2000) and to `--concurrency-cap` (default 512 sockets).
4. **Verify** during steady state: measure actual observed concurrency. If it drifts below $0.5 \times L_{target}$, adjust $\lambda$ once, then hold it fixed (fixed rate through the signal is required for clean phase attribution).
5. **Guard rails:**
   - If $W_{baseline}$ is so small that reaching $L_{target}$ needs more than `--max-rps`, emit `SC000 INSUFFICIENT_INFLIGHT` and advise `--slow-url`. The run continues but its verdict ceiling is `INCONCLUSIVE`.
   - If fewer than `--min-inflight-sample` (default 5) requests are actually classified `in_flight` at the signal, the verdict is **`INCONCLUSIVE`**, never `PASS`.
   - If baseline error rate exceeds 1%, abort in preflight — the service is already unhealthy and any result would be meaningless.

**This is the anti-false-PASS mechanism, and it is non-negotiable.**

### 7.6 Phase classification & shutdown profiles

With $T_0$ = signal timestamp and $W_a$ = accept window:

| Phase | Condition | Meaning |
|---|---|---|
| `warmup` | during warmup stage | recorded, excluded from scoring |
| `steady` | $T_{complete} < T_0$ | baseline correctness control group |
| `in_flight` | $T_{send} < T_0 < T_{complete}$ | **the critical bucket** — must all succeed |
| `post_signal` | $T_0 \le T_{send} \le T_0 + W_a$ | expectation depends on profile |
| `post_window` | $T_{send} > T_0 + W_a$ | must be refused |
| `at_sigkill` | still open when `SIGKILL` delivered | collateral damage, reported separately |

Connections are classified in parallel (`new` vs `reused`, and terminated-by-`FIN` vs `RST`), because a request can "succeed" on a connection the server then destroys — which still breaks the next request the client sends.

**Shutdown profiles** encode what "correct" means for the deployment model:

| Profile | Default accept window | New conns during window | New conns after window | Readiness flip |
|---|---|---|---|---|
| `standalone` *(default for `--pid` / managed command)* | 0s | either is acceptable | must be refused | warn if absent |
| `strict` | 0s | **must be refused immediately** | must be refused | warn if absent |
| `lame-duck` | 5s (`--accept-window`) | **must be accepted and succeed** | must be refused | **must flip ≤1s** |
| `kubernetes` *(default for `--k8s-pod`)* | `--prestop-sleep`, else 5s | **must be accepted and succeed** | must be refused | **must flip ≤1s** |
| `docker` *(default for `--docker`)* | 0s | either is acceptable | must be refused | warn if absent |

`--profile auto` (the default) selects by target kind. Profiles only change **severity and expectation**, never measurement — the same evidence is always collected, so a single run can be re-analysed under a different profile from its NDJSON output.

### 7.7 Connection-layer forensics

Request-level success/failure is not sufficient. Using `net/http/httptrace` plus a custom `DialContext`, each connection records: establishment time, reuse count, whether the response carried `Connection: close`, whether the peer closed with `FIN` or `RST`, and TLS handshake timing. This is what enables `SC004`, `SC009`, and `SC015`, and it is a genuine differentiator — no general load tester exposes this, and these are exactly the bugs that produce client-side errors even when your server-side metrics look clean.

The **independent TCP listener probe** dials the target port on a fixed cadence (default 20ms) *without* sending HTTP, purely to determine when `accept()` stopped. This decouples "is the listener open" from "does the app respond", which is precisely the distinction between `SC005` and `SC015`.

### 7.8 Timing, determinism & flake resistance

- All intervals use `time.Duration` on a **monotonic** clock; wall-clock timestamps appear only in the report for human correlation.
- The clock is an injectable interface so the analyzer and scheduler are fully unit-testable without sleeping.
- The scheduler is **open-model**: request dispatch times are pre-computed from the rate, so a slow server produces queueing (visible and measured) rather than silently reducing offered load — avoiding coordinated omission.
- `--seed` makes any randomised element (jitter, weighted scenario selection) reproducible.
- `--trials N` runs the full experiment N times. The reported verdict is the **worst** across trials; the report states consistency (`3/3 FAIL`, or `1/3 FAIL — flaky`). A flaky shutdown is itself a finding worth surfacing, not something to average away.
- Signal delivery precision is measured and reported; if the host is too loaded to deliver on time (>50ms skew), the run is flagged as low-confidence.

---

## 8. Diagnosis Engine — Failure Signature Catalogue

Each signature is an independent rule, individually unit-tested against synthetic timeline fixtures, and documented at `docs/signatures/SCxxx.md` plus `shutdowncheck explain SCxxx`. Severity `error` fails the run; `warn` deducts score only.

| ID | Name | Stage | Default severity | Detection rule | What it costs you in production |
|---|---|---|---|---|---|
| `SC000` | `INSUFFICIENT_INFLIGHT` | — | inconclusive | fewer than `min_inflight_sample` requests in flight at $T_0$ | Nothing — but the test proved nothing. Never reported as PASS. |
| `SC001` | `SIGTERM_IGNORED` | S1 | error | no listener/readiness/process change for the entire grace budget | Every deploy hard-kills the process; all in-flight work is destroyed. |
| `SC002` | `SIGKILL_REQUIRED` | S7 | error | process alive at $T_0 + grace$ | Orchestrator `SIGKILL`s you on every deploy; deploys are also slower. |
| `SC003` | `IN_FLIGHT_DROPPED` | S6 | error | ≥1 `in_flight` request failed | Users' in-progress requests fail on every deploy. The headline bug. |
| `SC004` | `ABRUPT_CONNECTION_RESET` | S5/S6 | error | connections ended with `RST` rather than `FIN` after $T_0$ | Client-side `ECONNRESET`; breaks non-idempotent retries. |
| `SC005` | `LISTENER_OPEN_AFTER_WINDOW` | S4 | error | TCP still accepted after $T_0 + W_a$ | You keep pulling in traffic you cannot finish; drain never completes. |
| `SC006` | `NO_DEREGISTRATION_WINDOW` | S3 | error (`lame-duck`/`kubernetes`), warn otherwise | listener closed within `dereg_min` (default 500ms) of $T_0$ | **502s at the ingress on every deploy.** The subtle, widespread bug. |
| `SC007` | `READINESS_NOT_FLIPPED` | S2 | error (`kubernetes`/`lame-duck`), warn otherwise | readiness endpoint healthy until exit | LB keeps routing to a dying instance for its full probe interval. |
| `SC008` | `READINESS_FLIP_SLOW` | S2 | warn | flipped later than `readiness_flip_budget` (default 1s) | Extra seconds of traffic sent to a terminating instance. |
| `SC009` | `KEEPALIVE_NOT_TERMINATED` | S5 | warn | no `Connection: close` / `GOAWAY` on post-signal responses while sockets were reused | Clients keep reusing a socket that is about to die → next request fails. |
| `SC010` | `SHUTDOWN_BUDGET_EXCEEDED` | S7 | error if `--max-shutdown-time` set | exit later than the user's declared budget (even if inside grace) | Slow deploys, long rollbacks, extended partial-availability windows. |
| `SC011` | `EARLY_EXIT` | S6 | error | process exited while `in_flight` requests were still pending | Process abandons work it accepted; silent data loss on write paths. |
| `SC012` | `PORT_HELD_AFTER_EXIT` | S7 | error | port still accepts connections after main process exit | Orphaned child/worker keeps the port; restarts fail or route to a zombie. |
| `SC013` | `NONZERO_EXIT_CODE` | S7 | warn | non-zero exit status after `SIGTERM` | Orchestrator records a crash; can trigger `CrashLoopBackOff` accounting. |
| `SC014` | `DRAIN_LATENCY_SPIKE` | S6 | warn | p99 during drain > `latency_spike_factor` (default 3×) baseline p99 | Timeouts upstream even where requests technically complete. |
| `SC015` | `ACCEPT_WITHOUT_RESPONSE` | S4 | error | TCP accepted post-signal but no HTTP response before timeout | Worst case for callers: requests hang to full timeout instead of failing fast. |
| `SC016` | `READINESS_FLAPPED` | S2 | warn | readiness went failing → healthy again during shutdown | LB re-registers a terminating instance; traffic returns to a dying pod. |
| `SC017` | `POST_KILL_TRAFFIC_LOSS` | S7 | derived | requests still in flight when `SIGKILL` landed | Quantifies exactly what the hard kill destroyed. |

### 8.1 Stack fingerprinting & remediation

Findings are paired with framework-specific fixes. The stack is determined by, in priority order: the `--stack` flag; the `Server` / `X-Powered-By` response headers; the container image name (Docker/K8s targets); the managed-command argv (`java -jar`, `dotnet`, `node`, `gunicorn`, `uvicorn`, …).

v1.0 remediation coverage: `go-net-http`, `node-express`, `node-fastify`, `python-gunicorn`, `python-uvicorn`, `java-spring-boot`, `dotnet-aspnetcore`, `ruby-puma`, `rust-axum`, and a generic fallback. Remediation content lives in versioned Markdown under `docs/signatures/` and is embedded into the binary via `go:embed` so `shutdowncheck explain` works fully offline.

---

## 9. Verdict, Score & Exit Codes

### 9.1 Verdict (rule-based, deterministic)

```
INCONCLUSIVE  if SC000 fired, or the run aborted, or in-flight sample < min_inflight_sample
FAIL          if any signature with effective severity=error fired,
              or any explicit gate threshold was breached
PASS          otherwise
```

Gate thresholds are evaluated alongside signatures: `--max-inflight-drop-pct` (default 0), `--max-shutdown-time`, `--min-score`, plus `--fail-on`/`--ignore` to promote or demote individual signature severities. With `--trials > 1`, the verdict is the worst across trials.

### 9.2 Shutdown Score (0–100, informational)

The score exists for badges, trend-watching and quick triage; **it never determines the verdict** unless `--min-score` is set explicitly. Deterministic deductions from 100, floored at 0:

| Condition | Deduction |
|---|---|
| `in_flight` failure ratio | −50 × ratio |
| `SC002` `SIGKILL_REQUIRED` | −40 |
| `SC001` `SIGTERM_IGNORED` | −40 |
| `SC015` `ACCEPT_WITHOUT_RESPONSE` | −20 |
| `SC005` post-window acceptance ratio | −20 × ratio |
| `SC006` `NO_DEREGISTRATION_WINDOW` | −15 |
| `SC007` `READINESS_NOT_FLIPPED` | −15 |
| `SC012` `PORT_HELD_AFTER_EXIT` | −15 |
| `SC004` reset-vs-FIN ratio | −10 × ratio |
| `SC011` `EARLY_EXIT` | −10 |
| `SC009`, `SC008`, `SC013`, `SC014`, `SC016` | −5 each |

Grades: **A** ≥ 90 · **B** ≥ 80 · **C** ≥ 70 · **D** ≥ 60 · **F** < 60. The exact weight table is published in the docs and versioned with `schema_version`, so a score is always reproducible and auditable.

### 9.3 Exit codes

| Code | Meaning |
|---|---|
| `0` | PASS |
| `1` | FAIL — shutdown defects detected |
| `2` | INCONCLUSIVE — the experiment was not valid; do not treat as pass |
| `3` | Usage / configuration error |
| `4` | Target error — could not start, reach, or signal the target |
| `5` | Internal error (bug in ShutdownCheck) |
| `130` | Interrupted |

Distinct codes matter: a CI pipeline should be able to treat `2` and `4` differently from a genuine `1`.

---

## 10. Output Formats

| Format | Flag | Purpose |
|---|---|---|
| Human | default | Timeline visualisation, phase table, connection table, findings with remediation. Colour auto-off when not a TTY or `NO_COLOR` is set. |
| JSON | `--format json` | Versioned, stable schema (Section 13). The integration contract. |
| JUnit XML | `--format junit` | One `<testcase>` per signature — findings appear natively in CI test UIs and PR annotations. |
| Markdown | `--format markdown` | Drop into `$GITHUB_STEP_SUMMARY` or a PR comment. |
| NDJSON | `--format ndjson` | Raw timeline event stream, one event per line, for custom analysis and re-analysis under a different profile. |
| SVG badge | `--badge <path>` | Score badge for the README. |

---

## 11. CLI Reference (v1.0)

```
shutdowncheck run [flags] [-- <command> [args...]]
shutdowncheck explain <SIGNATURE_ID>
shutdowncheck validate [--config <path>]     # lint config without running
shutdowncheck version
```

**Target (exactly one required)**
```
  -- <command> [args...]      spawn and manage the process (recommended)
  --pid <int>                 attach to an existing process
  --docker <name|id>          target a running container
```

**Probe**
```
  --url <url>                 required; the endpoint to load
  --method <verb>             default GET
  --header <k:v>              repeatable
  --body <string> | --body-file <path>
  --readiness-url <url>       readiness/health endpoint to poll (strongly recommended)
  --slow-url <url>            deliberately slow endpoint, used to guarantee in-flight load
  --listener-probe-interval   default 20ms
  --insecure                  skip TLS verification (prints a warning; never implicit)
```

**Load**
```
  --ensure-in-flight <n>      calibration goal, default 20
  --rps <n>                   fixed rate; disables calibration
  --max-rps <n>               calibration ceiling, default 2000
  --concurrency-cap <n>       max open sockets, default 512
  --warmup <dur>              default 3s
  --steady <dur>              pre-signal steady state, default 5s
  --request-timeout <dur>     default 10s
  --keep-alive                default true
  --seed <int>                reproducible jitter / scenario selection
```

**Termination**
```
  --signal TERM|INT|QUIT      default TERM
  --prestop-sleep <dur>       simulate a Kubernetes preStop hook
  --grace-period <dur>        default: target-specific (30s process, 10s docker)
  --enforce-sigkill           actually SIGKILL at grace expiry, default true
  --profile auto|standalone|strict|lame-duck|kubernetes|docker    default auto
  --accept-window <dur>       tolerated new-connection window, default per profile
```

**Gating**
```
  --max-inflight-drop-pct <f> default 0
  --max-shutdown-time <dur>   declared shutdown SLO
  --min-score <n>             fail below this score
  --fail-on <IDs>             promote signatures to error (comma-separated)
  --ignore <IDs>              demote signatures to warn
  --trials <n>                default 1; 3 recommended in CI
```

**Output & misc**
```
  --format human|json|junit|markdown|ndjson
  --output <path>             also write the report to a file
  --badge <path>              write an SVG score badge
  --capture-target-logs       interleave target stdout/stderr into the timeline
  --stack <id>                override stack detection for remediation hints
  --config <path>             default ./shutdowncheck.yaml if present
  --scenario <name>           select a named scenario from the config
  --quiet | --verbose | --no-color
```

---

## 12. Config File (`shutdowncheck.yaml`)

CLI flags override config; config overrides defaults. `shutdowncheck validate` lints it in CI.

```yaml
version: 1

defaults:
  profile: kubernetes
  grace_period: 30s
  ensure_in_flight: 20
  trials: 3
  capture_target_logs: true

scenarios:
  api:
    target:
      command: ["./bin/api", "--port", "8080"]
      ready:
        url: http://localhost:8080/readyz
        timeout: 30s
    probe:
      readiness_url: http://localhost:8080/readyz
      requests:                       # weighted mix; weights need not sum to 100
        - weight: 80
          url: http://localhost:8080/api/orders
          method: GET
        - weight: 20
          url: http://localhost:8080/api/orders
          method: POST
          headers:
            Content-Type: application/json
          body_file: ./testdata/order.json
    load:
      ensure_in_flight: 25
      steady: 8s
      request_timeout: 10s
    termination:
      prestop_sleep: 5s
      accept_window: 5s
    gate:
      max_inflight_drop_pct: 0
      max_shutdown_time: 15s
      min_score: 90
      ignore: [SC009]
```

---

## 13. JSON Report Schema (v1.0)

Emitted by `--format json`; the Go types live in the public `pkg/schema` package.

```json
{
  "schema_version": "1.0",
  "tool_version": "1.0.0",
  "verdict": "fail",
  "score": { "value": 34, "grade": "F", "weights_version": "1.0" },
  "run": {
    "started_at": "2026-09-03T10:15:18.000Z",
    "profile": "kubernetes",
    "seed": 42,
    "trials": { "total": 3, "failed": 3, "consistent": true }
  },
  "target": {
    "kind": "command",
    "label": "./bin/api --port 8080",
    "pid": 12345,
    "detected_stack": "dotnet-aspnetcore",
    "grace_period_ms": 5000
  },
  "probe": { "url": "http://localhost:8080/api/orders", "method": "POST",
             "readiness_url": "http://localhost:8080/readyz" },
  "load": {
    "calibrated": true,
    "rps": 340,
    "target_in_flight": 22,
    "observed_in_flight_at_signal": 22,
    "baseline_latency_ms": { "p50": 62, "p95": 88, "p99": 104 }
  },
  "timeline": {
    "signal_sent_at_ms": 0,
    "readiness_flipped_at_ms": null,
    "listener_closed_at_ms": 12,
    "sigkill_sent_at_ms": 5000,
    "process_exited_at_ms": 6400,
    "port_released_at_ms": 6410,
    "shutdown_duration_ms": 6400
  },
  "requests": {
    "total": 1364,
    "by_phase": {
      "steady":      { "count": 1024, "ok": 1024, "failed": 0 },
      "in_flight":   { "count": 22,   "ok": 16,   "failed": 6,
                       "failures": { "reset": 4, "timeout": 2 } },
      "post_signal": { "count": 318,  "ok": 0,    "failed": 318,
                       "failures": { "refused": 318 } },
      "post_window": { "count": 0,    "ok": 0,    "failed": 0 },
      "at_sigkill":  { "count": 0 }
    },
    "drain_latency_ms": { "p50": 71, "p95": 210, "p99": 980 }
  },
  "connections": {
    "opened": 128, "reused_after_signal": 41,
    "closed_with_connection_close": 0,
    "closed_by_fin": 87, "closed_by_rst": 41
  },
  "process": { "exit_code": 0, "terminated_by_signal": "SIGKILL", "port_held_after_exit": false },
  "findings": [
    {
      "id": "SC003", "name": "IN_FLIGHT_DROPPED", "stage": "S6", "severity": "error",
      "summary": "6 of 22 in-flight requests were destroyed during shutdown.",
      "evidence": { "in_flight_total": 22, "in_flight_failed": 6 },
      "impact": "In-progress user requests fail on every deploy.",
      "remediation_id": "SC003/dotnet-aspnetcore",
      "docs_url": "https://shutdowncheck.dev/signatures/SC003"
    }
  ],
  "gates": [
    { "name": "max_inflight_drop_pct", "threshold": 0, "actual": 27.3, "passed": false }
  ]
}
```

---

## 14. Security & Safety

ShutdownCheck sends signals to processes and generates traffic — it must be trustworthy enough to run in CI with production-adjacent credentials.

**Process & command safety**
- Subprocesses are executed via `exec.Command` with an **argv array only** — never `sh -c`, never string interpolation. Container/pod identifiers are validated against a strict allowlist regex before being passed as arguments (defence in depth against argument injection).
- Refuse to signal PID ≤ 1, and refuse the tool's own PID or process group, unless `--allow-unsafe-pid` is passed explicitly.
- Managed commands run in their own process group; cleanup kills the whole group so no orphans survive a failed run.
- `SIGKILL` is only ever delivered to a target the tool positively identified and is bounded by the grace period.

**Network safety**
- TLS certificate verification is **on by default**. `--insecure` is explicit, prints a prominent warning, and is recorded in the report.
- Only `http` and `https` target schemes are accepted; URLs are parsed and validated before use.
- No egress other than to the configured target. No update checks, no telemetry, no analytics — ever.

**Data handling**
- `Authorization`, `Proxy-Authorization`, `Cookie`, `Set-Cookie`, `X-Api-Key` and any header matching a configurable secret pattern are **redacted** in all reports, logs and NDJSON output.
- Request bodies are never echoed into reports; only a length and a hash.
- Captured target logs are size-capped (default 1MB) and scanned for the same redaction patterns.
- Report files are written with `0600` permissions by default, since they may contain internal URLs and hostnames.

**Resource safety**
- Per-request records are bounded (reservoir sampling above `--max-records`, default 100k) so a long run cannot exhaust memory.
- Response bodies are read with a hard byte cap and discarded; the tool never buffers full payloads.
- Socket count is capped by `--concurrency-cap`; the scheduler applies back-pressure rather than unbounded goroutine growth.
- Every network and process operation is `context`-bounded; the run has a hard wall-clock ceiling after which it aborts with exit code `5`.

**Supply chain**
- Pinned dependencies with a committed `go.sum`; minimal dependency budget (target: fewer than 10 direct dependencies).
- CI runs `go vet`, `staticcheck`, `gosec`, `govulncheck`, CodeQL, `go test -race`, and dependency review on every PR.
- Releases are reproducible, publish an SBOM, and are signed with `cosign` + SLSA provenance.
- Fuzz targets for all report parsers and config parsing.
- `SECURITY.md` with a coordinated disclosure policy and a private reporting channel.

---

## 15. Testing Strategy

A tool whose entire value proposition is "trust my verdict" has an unusually high testing bar. Four layers:

**L1 — Pure unit tests over synthetic timelines.** Because `analyze` is a pure function, every signature is tested by constructing a `Timeline` fixture in code and asserting the finding. Requirement: **every signature has at least one positive and one negative fixture**, and the suite fails if a signature has no test. Calibration maths, phase classification boundary conditions ($T_{send} = T_0$ exactly), scoring, and gate evaluation are all covered here. Target: >90% coverage in `analyze`, `timeline`, `config`, `report`.

**L2 — In-process HTTP server tests.** Go `httptest`-based servers that exhibit each pathological behaviour (hang, RST, instant close, no readiness flip) to test the prober and connection forensics without spawning processes. Fast, hermetic, race-detector enabled.

**L3 — Conformance suite (end-to-end, cross-language).** `test/conformance/` contains servers in **Go, Node.js, Python, Java, C#**, each with variants: `correct`, `ignores-signal`, `instant-close`, `no-readiness-flip`, `abrupt-reset`, `slow-drain`, `early-exit`, `orphan-child`. The E2E harness runs ShutdownCheck against every variant and asserts the exact expected signature set. This is the strongest possible evidence that the tool works black-box across stacks, and it doubles as documentation and demo material.

**L4 — Robustness.** `go test -race` on everything; fuzzing on config and report parsers; a soak test running 200 consecutive runs asserting zero leaked processes, sockets or goroutines; a flakiness test that runs the same fixture 50 times and requires an identical verdict every time. **Determinism is a tested property, not an aspiration.**

CI matrix: Linux and macOS on every PR (Docker-target tests Linux-only); Windows for build + Docker-target tests once D8 lands.

---

## 16. Distribution & Adoption

Distribution is a first-class feature, not an afterthought — the tool's whole premise is "drop it in anywhere."

- **Binaries** for linux/macos/windows × amd64/arm64 via GoReleaser, attached to GitHub Releases, signed and with SBOMs.
- **`go install github.com/<org>/shutdowncheck/cmd/shutdowncheck@latest`**
- **Homebrew tap**, **Scoop** manifest, and an install script (`curl … | sh`, with checksum verification documented).
- **Container image** (`ghcr.io/<org>/shutdowncheck`), distroless, multi-arch — the zero-install path for CI.
- **GitHub Action** in `action/`, published to the Marketplace:

```yaml
- uses: <org>/shutdowncheck-action@v1
  with:
    config: shutdowncheck.yaml
    scenario: api
    comment-on-pr: true
```

  It runs the check, uploads the JSON report as an artifact, writes the Markdown report to the job summary, and optionally posts/updates a PR comment.
- **Docs site** (`shutdowncheck.dev`) built from `docs/` — one page per signature is the long-tail SEO engine ("connection reset during kubernetes deploy" should land people on `SC004`).
- **README** with an asciinema/GIF demo showing a broken server failing and the fixed one passing, plus the score badge.

---

## 17. Repository & Governance

- **License:** Apache-2.0 (patent grant matters for corporate adoption; more permissive-friendly than GPL for an infra CLI).
- `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, `SECURITY.md`, `docs/adr/` for decisions.
- Conventional Commits + semantic-release; `good first issue` / `help wanted` triage from day one.
- Issue templates including a **"my service is misdiagnosed"** template that requests the NDJSON timeline — turning bug reports into ready-made analyzer fixtures.
- Public roadmap in GitHub Projects; every deferred item from Section 6.9 is an open issue so contributors can self-serve.

---

## 18. Post-1.0 Roadmap (directional)

- **v1.1** — Kubernetes pod targeting (in-cluster and via kubeconfig), Docker Compose service targeting, real Endpoints-de-registration observation.
- **v1.2** — HTTP/2 support with `GOAWAY` verification (a genuinely under-tested area); native Windows termination model.
- **v1.3** — gRPC probing, including streaming-RPC drain semantics.
- **v2.0** — Local run history and regression comparison (`shutdowncheck compare`), answering "did shutdown behaviour regress since last release?" — rule-based and comparative, still no ML.
- **v2.x** — Rolling-deploy simulation: run N instances behind an embedded load balancer, terminate them in sequence, and assert end-to-end zero-error deploys. This is the natural end state of the product thesis.
- **Unscheduled** — optional companion libraries implementing correct termination per framework (never required by the core tool); message-queue consumer drain verification.

---

## 19. Success Criteria

**Technical (must all hold at v1.0):**
- Every signature in Section 8 is triggerable and asserted by at least one conformance fixture in at least two languages.
- Zero false PASS: no configuration exists in which a known-broken conformance server yields `PASS`.
- Verdict stability: 50 repeated runs of the same fixture produce an identical verdict.
- Default run completes in under 30 seconds; the binary is under 15MB; direct dependencies fewer than 10.
- Clean run of `go test -race ./...`, `staticcheck`, `gosec`, `govulncheck` in CI.

**Adoption (12 months post-launch, directional):**
- Real external usage: stars and — more meaningfully — issues and PRs from people with no connection to the author.
- The GitHub Action installed in repositories outside the author's own.
- At least one signature documentation page ranking for its symptom search terms.
- The `docs/signatures/` pages cited by someone else's blog post or Stack Overflow answer.

**Portfolio:** a codebase that stands as evidence of systems thinking — a pure, testable analysis core; a clean plugin boundary for targets; deliberate handling of a genuinely subtle distributed-systems race; and a security posture appropriate for an infrastructure tool.

---

## 20. Remaining Open Questions

### Resolved

1. **`SC006` default severity under `--profile auto`.** ✅ *Resolved in Phase 4.* Severity is profile-dependent: `error` under `lame-duck` and `kubernetes`, `warn` under `standalone` and `docker`, and `info` under `strict` — where closing the listener immediately is the requirement rather than the defect. The same evidence therefore reaches opposite conclusions under different deployment models, which is asserted directly by `TestProfileChangesTheVerdictForTheSameEvidence`.
2. **Score weight calibration.** ✅ *Resolved in Phase 4.* The Section 9.2 table is pinned by `TestScoreBandsAreCalibrated`, which fixes the intended bands: a correct shutdown scores 100 (A), a single minor flaw stays in A, a service that closes its listener too early lands in B, and a thoroughly broken one lands in F. Changing a weight now fails that test rather than silently rescaling every published score.
3. **Readiness auto-discovery.** ✅ *Resolved in Phase 3.* The tool does **not** guess at `/readyz`, `/healthz` or similar. Probing only happens against an explicitly configured endpoint, and `SC007` never fires when readiness was not probed. Guessing would mean an unconfigured probe could be mistaken for evidence, and absence of data must never be read as data.
4. **NDJSON re-analysis subcommand.** ✅ *Resolved in Phase 5 — built.* `shutdowncheck analyze run.ndjson --profile kubernetes` re-judges a recorded run without repeating it. It was cheap because analysis is pure, and it is what makes the separation of measurement from interpretation concrete: the same recording passes under `standalone` and fails under `kubernetes`, which is asserted by `TestAnalyzeReJudgesRecordedEvidence`. It also gives maintainers a way to reproduce a misdiagnosis report exactly, from the NDJSON the reporter attached.

### Still open

5. **Bundled conformance servers.** Should `shutdowncheck demo` embed a tiny broken/fixed Go server so a first-time user can see a failing run with zero setup? Strong for onboarding; small binary-size cost. *(Decide in Phase 6.)*

---

*End of v0.2. Implementation proceeds against `DEVELOPMENT-PLAN.md`.*

---

*End of v0.1 draft. Next step: work through the open questions above, lock the v1 feature list, then move to implementation planning (repo structure, module breakdown, test strategy).*
