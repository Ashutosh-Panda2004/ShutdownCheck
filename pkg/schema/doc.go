// Package schema defines the public, versioned JSON report emitted by
// shutdowncheck. It is deliberately importable by third parties so that CI
// systems and dashboards can consume reports as typed data.
//
// # Compatibility
//
// Every change here is a compatibility decision. Additive fields bump the minor
// SchemaVersion; removing or repurposing a field requires a major bump and an
// ADR. See spec section 13.
//
// # Purity constraint
//
// Like internal/analyze, this package must perform no I/O and must not read the
// clock. Enforced by TestAnalyzeAndSchemaArePure in test/architecture.
//
// Implemented in Phase 1.
package schema
