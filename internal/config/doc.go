// Package config models shutdowncheck.yaml: defaults, named scenarios, probe
// and load settings, termination settings, and gate thresholds. It resolves the
// precedence chain (CLI flags > config file > built-in defaults) and derives the
// analysis Policy from the selected shutdown profile.
//
// Validation is strict and errors are actionable: a bad config should say what
// is wrong and what a valid value looks like, never just "invalid input".
//
// Implemented in Phase 1.
package config
