// Package remediate turns a finding into advice a developer can act on.
//
// Stack detection is layered: the --stack flag, then the Server and
// X-Powered-By response headers, then the container image name, then the
// launch command. It only ever selects better wording, never a verdict.
//
// Documentation is embedded with go:embed so `shutdowncheck explain` works with
// no network access, which matters because the tool runs inside CI runners and
// on locked-down infrastructure. Any signature without a hand-written page gets
// one generated from the catalogue, so no finding is ever unexplained.
package remediate
