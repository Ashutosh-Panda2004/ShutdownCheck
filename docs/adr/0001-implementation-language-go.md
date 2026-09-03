# ADR-0001: Implement ShutdownCheck in Go

- **Status:** Accepted
- **Date:** 2026-09-03

## Context

ShutdownCheck is a black-box tool. Its central claim is that it tests any backend service identically regardless of that service's language or framework. It is intended to be dropped into arbitrary CI environments and developer machines with no setup.

The author's strongest language is C#/.NET, which makes that a genuine candidate. The decision affects distribution, the concurrency model, and how signals and process trees are handled.

## Decision

Implement in Go.

## Consequences

Gained:

- A single static binary per platform with no runtime on the host. A tool whose premise is "works regardless of your stack" cannot credibly require its own runtime to be installed first.
- Trivial cross-compilation to linux/darwin/windows on amd64/arm64 from one machine.
- First-class `os/exec` and `syscall` support for signals and process groups, which is the core of what the tool does.
- `net/http/httptrace` in the standard library, which is what makes connection-layer forensics (`SC004`, `SC009`, `SC015`) possible without a third-party HTTP stack.
- Ecosystem alignment: infrastructure CLIs are expected to be Go binaries, which lowers the barrier for outside contributors.

Accepted costs:

- The author writes Go less fluently than C#, so early velocity is lower and the code must be reviewed more carefully for non-idiomatic patterns.
- Go's error handling and lack of generics-heavy abstractions make some modelling more verbose than the C# equivalent.

Mitigation: the conformance suite includes a C#/ASP.NET Core fixture and the remediation catalogue covers ASP.NET Core in depth, so .NET expertise remains directly useful to the project.

## Alternatives considered

- **C# with a self-contained deployment.** Rejected: self-contained .NET binaries are tens of megabytes even after trimming, publishing per-RID adds friction, and POSIX signal and process-group handling is markedly less direct. The distribution penalty falls on every user, whereas the language-familiarity penalty falls only on the author.
- **Rust.** Comparable distribution story and excellent correctness properties, but a steeper learning curve for the author and a smaller pool of likely contributors for this category of tool. No advantage that outweighs those.
