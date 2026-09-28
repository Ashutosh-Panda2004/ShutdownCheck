# 90-Second Demo Script: Fail → Explain → Fix → Pass

## Setup (before recording)

```bash
# Build the tool
go build -o /tmp/shutdowncheck ./cmd/shutdowncheck

# Build the demo server (deliberately broken by default)
go build -o /tmp/demo-server ./test/conformance/go
```

## The Script (90 seconds)

### 0:00-0:15 — The Problem (15s)

> "Every time you deploy, Kubernetes sends SIGTERM to your pods. If your service mishandles the 30-second window before SIGKILL, **every deploy drops production traffic**. And you won't see it in tests, because it's a race — whether a request dies depends on whether it was mid-flight at the exact microsecond the signal landed."

### 0:15-0:35 — FAIL (20s)

```bash
# Run against the broken demo server
/tmp/shutdowncheck run --url http://127.0.0.1:8080/work \
  --profile kubernetes --grace-period 6s \
  -- /tmp/demo-server -addr 127.0.0.1:8080 -mode instant-close
```

> "This server closes its listener the instant it gets SIGTERM — which is what most tutorials tell you to do. ShutdownCheck says FAIL. It found SC006: the listener closed before de-registration could propagate, so the ingress will 502. And SC007: readiness never flipped, so the load balancer kept sending traffic to a dying pod."

**Show the output:** VERDICT: FAIL, SC006 and SC007 in red.

### 0:35-0:55 — EXPLAIN (20s)

```bash
/tmp/shutdowncheck explain SC006
```

> "SC006 means: you closed the listener, but Kubernetes hasn't updated the routing tables yet. There's a propagation delay — usually 1-2 seconds. During that window, the ingress still thinks your pod is healthy and sends traffic, which gets 'connection refused'. The fix isn't to close faster; it's to **fail readiness first, keep serving, then close**."

**Show the explain output:** the "how to fix" section with framework-specific code.

### 0:55-1:15 — FIX (20s)

> "The fixed server does the lame-duck: on SIGTERM, it immediately fails readiness, keeps serving for 2 seconds while the load balancer drains, then closes the listener and drains in-flight requests."

```bash
# Run against the fixed server (mode=correct)
/tmp/shutdowncheck run --url http://127.0.0.1:8081/work \
  --profile kubernetes --grace-period 6s \
  -- /tmp/demo-server -addr 127.0.0.1:8081 -mode correct
```

### 1:15-1:30 — PASS (15s)

**Show the output:** VERDICT: PASS, score 95/100, green.

> "PASS. Zero dropped requests. That's the difference between a deploy that causes 502s and one that doesn't — and ShutdownCheck tells you which one you have before you merge."

## Backup Recording Plan

If the live demo fails (network flake, port conflict):

1. **Pre-record the FAIL output** to `demo/fail.txt`:
   ```bash
   /tmp/shutdowncheck run ... --mode instant-close > demo/fail.txt 2>&1 || true
   ```

2. **Pre-record the PASS output** to `demo/pass.txt`:
   ```bash
   /tmp/shutdowncheck run ... --mode correct > demo/pass.txt 2>&1 || true
   ```

3. **Pre-generate the HTML reports**:
   ```bash
   /tmp/shutdowncheck run --format html --output demo/fail.html ... --mode instant-close || true
   /tmp/shutdowncheck run --format html --output demo/pass.html ... --mode correct || true
   ```

4. During the demo, `cat` the pre-recorded files instead of running live. The audience can't tell the difference, and you eliminate flake risk.

## Recording Commands

```bash
# Terminal 1: start the broken server
/tmp/demo-server -addr 127.0.0.1:8080 -mode instant-close

# Terminal 2: run the check (record this)
clear && /tmp/shutdowncheck run --url http://127.0.0.1:8080/work \
  --readiness-url http://127.0.0.1:8080/readyz \
  --profile kubernetes --grace-period 6s --accept-window 1s \
  -- /tmp/demo-server -addr 127.0.0.1:8080 -mode instant-close
```
