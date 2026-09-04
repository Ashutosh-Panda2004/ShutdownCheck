---
title: "The seven stages of correct termination"
description: "Why graceful shutdown advice is wrong under Kubernetes, and what a service actually has to do between SIGTERM and exit."
---

# The seven stages of correct termination

Every deploy does the same thing to your service: `SIGTERM`, wait, `SIGKILL`.
So does every autoscale-down, every node drain, every spot reclaim, every
rolling restart. If your service mishandles the window in between, **every one
of those events silently drops production traffic.**

It rarely shows up in testing, because the bug is a race. Whether a request dies
depends on whether it happened to be in flight at the microsecond the signal
landed. Most teams find out from a customer complaint, years later, and never
trace it back.

This page is the model ShutdownCheck verifies against. Every check it runs maps
to exactly one stage below, and every diagnosis names the stage that broke.

## The advice almost everyone follows is wrong

Search for "graceful shutdown" in any language and you will find this:

> On `SIGTERM`, immediately stop accepting new connections, finish in-flight
> requests, then exit.

Under Kubernetes, and behind most load balancers, **that is a bug.**

De-registration is asynchronous. When a pod is deleted, two things happen at the
same time:

1. the kubelet sends `SIGTERM` to your container, and
2. the endpoints controller starts removing the pod from Service endpoints,
   which then has to propagate to every kube-proxy, ingress and sidecar in the
   cluster.

Nothing orders those. Nothing waits for the second to finish. Your process
routinely receives `SIGTERM` while traffic is still being routed to it.

So a service that closes its listener the instant the signal arrives produces a
burst of `connection refused`, and your ingress turns those into **502s for real
users**. The service did graceful shutdown by the book and still dropped
traffic.

The fix is a **lame-duck window**: fail readiness immediately so the control
plane starts removing you, but *keep serving* for the propagation delay, and
only then close the listener and drain.

This is the single most important idea on this page, and it is why ShutdownCheck
has [profiles](#profiles-decide-what-correct-means) rather than one fixed
definition of correct.

## The seven stages

| | Stage | What must happen | Signatures |
| --- | --- | --- | --- |
| **S1** | Signal received | The process reacts to `SIGTERM` at all | [SC001](signatures/SC001.md) |
| **S2** | Readiness flipped | The health endpoint starts failing immediately | [SC007](signatures/SC007.md), [SC008](signatures/SC008.md), [SC016](signatures/SC016.md) |
| **S3** | Lame-duck window | Keep accepting new connections while de-registration propagates | [SC006](signatures/SC006.md) |
| **S4** | Listener closed | Stop accepting once the window has elapsed | [SC005](signatures/SC005.md), [SC015](signatures/SC015.md) |
| **S5** | Connection close signalled | `Connection: close` on keep-alive responses, `GOAWAY` on HTTP/2 | [SC004](signatures/SC004.md), [SC009](signatures/SC009.md) |
| **S6** | In-flight work drained | Every request already accepted runs to completion | [SC003](signatures/SC003.md), [SC011](signatures/SC011.md), [SC014](signatures/SC014.md) |
| **S7** | Clean exit inside budget | Exit before the grace period expires, with no orphans | [SC002](signatures/SC002.md), [SC010](signatures/SC010.md), [SC012](signatures/SC012.md), [SC013](signatures/SC013.md) |

### S1 — Signal received

The floor. A process that ignores `SIGTERM` gets `SIGKILL` when the grace period
expires, and `SIGKILL` cannot be handled: every in-flight request dies mid-write
and every connection is reset.

The usual cause is not carelessness — it is a shell. `CMD ./app` in a Dockerfile
runs `/bin/sh -c ./app`, so PID 1 is the shell, and it does not forward signals
to your process. Your handler is correct and never runs. Use the exec form,
`CMD ["./app"]`.

### S2 — Readiness flipped

Fail readiness **first**, before anything else, because this is what starts the
clock on de-registration. Every second you delay is a second longer that traffic
keeps arriving.

Readiness must also *latch*. A health check that goes back to reporting healthy
during shutdown re-adds you to the load balancer, which is worse than never
having flipped at all.

### S3 — Lame-duck window

Keep serving. This is the stage that contradicts the common advice and the one
most services get wrong.

The window needs to cover the propagation delay of your infrastructure — how
long it takes the removal to reach every proxy. It is measured in seconds, not
milliseconds, and it is a property of your cluster rather than your code.

If you have never measured it, 5 to 15 seconds is a common starting range.

### S4 — Listener closed

Once the window has elapsed, stop accepting. New connections should be refused
cleanly, not accepted and left hanging — a socket that is accepted and never
answered is worse than a refusal, because the client waits for a timeout instead
of failing over immediately.

### S5 — Connection close signalled

Keep-alive means a client may be holding an idle connection to you and intending
to reuse it. If you simply close, the request it sends next is lost in a race.

Tell it instead: `Connection: close` on the responses you send during shutdown,
or `GOAWAY` on HTTP/2. Then the client retires the connection on its own terms.

The symptom of getting this wrong is a connection reset — an `RST` rather than
an orderly `FIN` — which client libraries often will not retry, because a reset
mid-request is not safely idempotent.

### S6 — In-flight work drained

Every request you already accepted must finish. This is the stage people think
of as "graceful shutdown", and it is the one most frameworks handle for you —
`Server.Shutdown` in Go, Tomcat's graceful shutdown, `server.close()` in Node.

The common failure is a drain that runs *longer* than the grace period, at which
point `SIGKILL` arrives and the drain was pointless. Your drain deadline must be
shorter than your orchestrator's.

### S7 — Clean exit inside budget

Exit, with status `0`, before the grace period expires — and leave nothing
behind. A child process that outlives its parent and keeps the listening socket
open means the port stays busy after the orchestrator believes the container is
gone, and the next deploy fails to bind.

## Profiles decide what "correct" means

Whether closing the listener immediately is a defect depends entirely on what is
in front of your service. The same recording reaches opposite conclusions:

| Profile | Lame-duck window | Correct behaviour |
| --- | --- | --- |
| `kubernetes` | required | Flip readiness, keep serving, then drain |
| `lame-duck` | required | As above, for non-Kubernetes load balancers |
| `standalone` | optional | Nothing is routing to you; closing immediately is fine |
| `strict` | forbidden | Stop accepting immediately — the requirement, not the defect |
| `docker` | optional | `docker stop` semantics, 10s grace by default |

This is why ShutdownCheck separates measurement from interpretation. A run is
recorded once and can be re-judged later without repeating it:

```console
$ shutdowncheck run ... --format ndjson --output run.ndjson
$ shutdowncheck analyze run.ndjson --profile standalone   # PASS
$ shutdowncheck analyze run.ndjson --profile kubernetes   # FAIL: SC006
```

Same evidence. Different deployment model. Different answer. Neither is wrong.

## Why this is hard to test by hand

The bug only appears when a request is genuinely in flight at the moment the
signal lands. Send the signal to an idle service and everything looks perfect,
which is why this survives code review and staging.

That is the whole reason ShutdownCheck calibrates load first: it establishes a
request rate that *guarantees* work is in flight when it signals, so the race is
forced rather than hoped for. If it cannot establish that, it reports
[SC000](signatures/SC000.md) and returns `INCONCLUSIVE` — never a pass.

## See it

```console
$ shutdowncheck demo
```

That runs a real check against a deliberately broken service, showing the S3 and
S2 failures described above. Nothing is simulated: it is a real process,
receiving a real signal, measured by the same code path as any other run.
