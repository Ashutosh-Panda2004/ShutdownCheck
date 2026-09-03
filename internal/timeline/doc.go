// Package timeline defines the immutable, monotonically-ordered record of
// everything observed during a run: request results, connection lifecycle,
// readiness samples, listener-accept samples, the termination signal, process
// exit, and target log lines.
//
// It is the single source of truth that every other package either writes into
// (probes, load engine, targets) or reads from (analyze, report). All event
// offsets are monotonic durations from run start; wall-clock times exist only
// for human correlation in the final report.
//
// Implemented in Phase 1.
package timeline
