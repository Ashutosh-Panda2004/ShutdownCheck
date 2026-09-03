// Package run orchestrates a single experiment: the state machine that moves
// through preflight, target start, readiness wait, warmup, calibration, steady
// state, optional preStop hold, signal, observation, SIGKILL escalation,
// post-exit checks, and analysis.
//
// Every state transition is recorded on the timeline so a report can always
// explain what the tool itself was doing at any instant. Cleanup is
// unconditional, including on panic or interruption of shutdowncheck itself.
//
// Implemented in Phase 3.
package run
