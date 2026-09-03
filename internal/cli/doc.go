// Package cli wires the command-line surface: run, explain, validate and
// version, along with flag parsing, config merging, and the exit-code taxonomy
// from spec section 9.3.
//
// Command implementations live here rather than in cmd/ so that they are
// testable without building a binary; cmd/shutdowncheck contains no logic.
//
// Implemented in Phase 5.
package cli
