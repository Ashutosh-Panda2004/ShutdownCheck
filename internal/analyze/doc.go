// Package analyze turns a recorded timeline into a verdict: it classifies every
// request and connection into a shutdown phase, evaluates the failure signature
// catalogue (SC000-SC017), applies gate thresholds, and computes the shutdown
// score.
//
// # Purity constraint
//
// This package must remain a pure function of its inputs: Analyze(Timeline,
// Policy) -> Report. It must perform no I/O and must never read the clock.
// This is what makes every verdict reproducible and unit-testable against
// synthetic timeline fixtures with no network or process involvement.
//
// The constraint is mechanically enforced by TestAnalyzeAndSchemaArePure in
// test/architecture. Do not work around it.
//
// Implemented in Phase 4.
package analyze
