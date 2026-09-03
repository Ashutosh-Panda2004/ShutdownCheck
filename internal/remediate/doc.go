// Package remediate maps a failure signature plus a detected stack to a concrete
// fix. Stack detection is layered: the --stack flag, then the Server and
// X-Powered-By response headers, then the container image name, then the
// managed-command argv.
//
// Remediation content lives as Markdown under docs/signatures and is embedded
// into the binary so `shutdowncheck explain` works with no network access.
//
// Implemented in Phase 4.
package remediate
