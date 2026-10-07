# ShutdownCheck FAQ

## How is this different from Gremlin, Litmus, or Chaos Mesh?

Gremlin, Litmus, and Chaos Mesh are **chaos engineering platforms**: they inject faults (kill pods, partition networks, consume CPU) to test resilience. They're powerful but heavyweight — they require agents, dashboards, and often a paid tier.

ShutdownCheck is **not chaos engineering**. It's a **pre-deployment verification tool** — like a linter for shutdown behavior. It answers one specific question: "Will this service drop traffic when the orchestrator terminates it?" It does this by:

1. Sending SIGTERM to your service (the same signal Kubernetes/docker sends).
2. Measuring what happens to in-flight requests during the drain window.
3. Telling you exactly which stage of the seven-stage shutdown sequence broke, and how to fix it in your framework.

Use Chaos Mesh to test "what happens if the network partitions." Use ShutdownCheck to verify "my deploy won't cause 502s" — in CI, before you merge.

## What about service-mesh draining (Istio, Linkerd)?

Service meshes handle **traffic shifting** (draining connections at the proxy layer), but they don't fix **application-level shutdown bugs**. If your app:

- Closes its listener before de-registration propagates → the mesh can't save you from refused connections.
- Doesn't fail readiness on SIGTERM → the mesh keeps sending traffic to a dying pod.
- Exits while requests are in flight → the mesh sees a broken pipe.

ShutdownCheck verifies the **application** does its part. The mesh handles the network; your app still needs to handle SIGTERM correctly.

## What's the business model?

ShutdownCheck is open source (MIT). There's no SaaS, no paid tier, no telemetry. If it becomes useful:

- **Enterprise**: Support contracts for teams that want "someone to call" when their deploy pipeline breaks.
- **CI integration**: A managed GitHub Action with historical tracking ("your shutdown score dropped from 95 to 70").
- **Training**: Workshops on graceful shutdown for platform teams.

But right now, it's just a tool. Use it.

## Why not just use `docker stop` and see what happens?

You can, but you won't learn *why* it broke. `docker stop` gives you a pass/fail. ShutdownCheck gives you:

- Which of the seven stages failed (readiness flip? listener close? drain?).
- How many requests were affected, and what happened to them (refused? reset? timeout?).
- Framework-specific fix guidance (`shutdowncheck explain SC006`).

It's the difference between "it broke" and "here's the exact line to change."

## Does it work with my framework?

If your framework runs on Linux and handles SIGTERM, yes. Verified with Go (`net/http`), Node.js, and Python. The seven stages are framework-agnostic — they're about the contract between your app and the orchestrator, not about specific APIs.

## Why are SC004 and SC005 judged differently for Docker targets?

Because for a container, the client connections and the published port belong to Docker's proxy rather than your service. The proxy holds connections open while your service drains, then resets all of them when the container dies — however cleanly your service closed its own side — so SC004 (abrupt resets) is not judged for Docker targets; the counts still appear in the report's statistics, and what a reset destroys is judged as usual through the in-flight requests it kills (SC003). SC005 (listener still accepting) is judged, but against when the published port actually stopped accepting, with a two-second allowance for signal delivery through the daemon and the proxy winding down; a port still accepting well past its window plus that allowance is a service that never closed, and the run's answered requests show it.

## Is this production-ready?

It's alpha. The core analysis is solid (verified against 14 conformance scenarios in Go), but:
- Kubernetes targeting is not yet implemented (use the `kubernetes` profile with a process target).
- Node.js and Python conformance fixtures have known issues.
- The HTML report is new.

Use it to learn, not as a merge gate you depend on. Yet.
