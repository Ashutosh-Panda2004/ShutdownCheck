// Command shutdowncheck verifies that a service drains in-flight requests
// correctly when it is terminated, and reports which stage of shutdown failed.
//
// See shutdowncheck-spec.md for the design this implements.
package main

import (
	"os"

	"github.com/shutdowncheck/shutdowncheck/internal/cli"
)

// Overridden at release time via -ldflags.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	cli.Version, cli.Commit, cli.Date = version, commit, date
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
