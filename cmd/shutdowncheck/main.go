// Command shutdowncheck verifies that a service drains in-flight requests
// correctly when it is terminated, and reports which stage of shutdown failed.
//
// See shutdowncheck-spec.md for the design this implements.
package main

import (
	"fmt"
	"os"
)

// Overridden at release time via -ldflags.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// exitUsage matches the taxonomy in spec section 9.3; the full set of codes
// arrives with the real CLI in Phase 5.
const exitUsage = 3

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Printf("shutdowncheck %s (commit %s, built %s)\n", version, commit, date)
			return
		}
	}

	fmt.Fprintln(os.Stderr, "shutdowncheck: this is a Phase 0 scaffold; no commands are implemented yet.")
	fmt.Fprintln(os.Stderr, "Try: shutdowncheck version")
	os.Exit(exitUsage)
}
