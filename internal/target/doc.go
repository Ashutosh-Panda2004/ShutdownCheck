// Package target abstracts "a thing that can be terminated and observed":
// an existing process (by PID), a process shutdowncheck spawns and owns, a
// Docker container, and later a Kubernetes pod.
//
// Keeping every target behind one interface is what makes new target kinds
// additive rather than invasive; no other package should ever branch on target
// kind.
//
// Safety rules that implementations must uphold: build commands as an argv slice
// and never via a shell; validate externally-supplied identifiers before they
// reach argv; refuse to signal PID <= 1 or shutdowncheck's own process group;
// and guarantee cleanup of spawned process groups on every exit path.
//
// Implemented in Phase 3 (process, command) and Phase 7 (docker).
package target
