// Package load generates rate-controlled traffic against the target.
//
// The scheduler is open-model: dispatch times are precomputed from the arrival
// rate rather than derived from response completions, so a slowing server
// produces measurable queueing instead of a silently reduced offered rate
// (coordinated omission).
//
// It also owns auto-calibration, which derives the request rate needed to hold a
// target number of requests in flight at the moment of termination. Without
// that guarantee a run can trivially produce a meaningless PASS, so calibration
// failure downgrades the verdict to INCONCLUSIVE rather than PASS.
//
// Implemented in Phase 2.
package load
