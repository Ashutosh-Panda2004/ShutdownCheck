// Command shutdowncheck verifies that a service drains in-flight requests
// correctly when it is terminated, and reports which stage of shutdown failed.
//
// See shutdowncheck-spec.md for the design this implements.
package main

import (
	"os"
	"runtime/debug"

	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/cli"
)

// Overridden at release time via -ldflags.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	cli.Version, cli.Commit, cli.Date = buildInfo()
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}

// buildInfo prefers the linker-supplied values and falls back to what the
// toolchain embeds, so a binary from `go install ...@v1.2.3` reports v1.2.3
// rather than "dev" — there are no ldflags on that path.
func buildInfo() (v, c, d string) {
	v, c, d = version, commit, date

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return v, c, d
	}

	var revision, vcsTime string
	var modified bool
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.time":
			vcsTime = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}

	if v == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
		v = info.Main.Version
	}
	if c == "none" && revision != "" {
		c = shortCommit(revision)
		// Only ever qualifies a commit we read from VCS; a release build states
		// its own, and appending to that would misreport it.
		if modified {
			c += "-dirty"
		}
	}
	if d == "unknown" && vcsTime != "" {
		d = vcsTime
	}
	return v, c, d
}

func shortCommit(rev string) string {
	const shortLen = 12
	if len(rev) > shortLen {
		return rev[:shortLen]
	}
	return rev
}
