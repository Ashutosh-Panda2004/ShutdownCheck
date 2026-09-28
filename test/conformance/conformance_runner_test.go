//go:build !windows

// Package conformance drives ShutdownCheck against the same set of shutdown
// defects implemented in several languages.
//
// This is where "works regardless of your stack" stops being a claim. The same
// mistake written in Go, Node and Python must produce the same verdict and the
// same signatures, and every correct implementation must pass cleanly. A broken
// server that yields a pass anywhere would be a false negative, which is the one
// failure this project cannot tolerate.
//
// The servers need POSIX signals, so the runs here are Unix-only. The contract
// they are held to lives in scenarios_test.go and is checked everywhere.
package conformance

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/cli"
	"github.com/Ashutosh-Panda2004/ShutdownCheck/pkg/schema"
)

// freePort reserves and releases a port so the server can be told exactly where
// to listen and the harness knows the URL before anything starts.
func freePort(t *testing.T) int {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func TestConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("conformance spawns real servers in several languages; run without -short")
	}

	for _, lang := range languages() {
		t.Run(lang.name, func(t *testing.T) {
			argv, skip := lang.prepare(t)
			if skip != "" {
				// Locally a missing runtime is a fact of life. In CI it means
				// the suite is quietly proving less than it advertises, and a
				// green tick would be misleading.
				if os.Getenv("CI") != "" {
					t.Fatalf("%s is unavailable in CI, so the cross-stack claim is untested: %s",
						lang.name, skip)
				}
				t.Skip(skip)
			}

			for _, s := range scenarios {
				if !lang.modes[s.mode] {
					continue
				}
				t.Run(s.mode+"/"+s.profile, func(t *testing.T) {
					runScenario(t, argv, s)
				})
			}
		})
	}
}

func runScenario(t *testing.T, serverArgv []string, s scenario) {
	t.Helper()

	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	args := []string{
		"run",
		"--url", "http://" + addr + "/work",
		"--readiness-url", "http://" + addr + "/readyz",
		"--profile", s.profile,
		"--grace-period", s.grace,
		"--ensure-in-flight", "6",
		"--warmup", "600ms",
		"--steady", "700ms",
		"--request-timeout", "1s",
		"--no-color",
		"--no-timeline",
	}
	if s.accept != "" {
		args = append(args, "--accept-window", s.accept)
	}

	args = append(args, "--")
	args = append(args, serverArgv...)
	args = append(args, "-addr", addr, "-mode", s.mode)

	var stdout, stderr bytes.Buffer
	code := cli.Main(args, &stdout, &stderr)
	assertScenario(t, s, code, stdout.String(), stderr.String())
}

// assertScenario is the contract, shared by every way of reaching a target.
// A Docker container and a local process must be judged identically, so they
// must be checked by identical code.
func assertScenario(t *testing.T, s scenario, code int, output, stderrText string) {
	t.Helper()

	// The single most important property in the whole project: a server with a
	// known defect must never produce a pass. "Known defect" is
	// profile-relative: instant-close is defective under kubernetes but
	// compliant under strict, so a scenario that explicitly expects a pass is
	// exempt from this guard.
	if s.verdict != schema.VerdictPass && s.mode != "correct" && code == schema.ExitPass {
		t.Fatalf("a broken server produced a PASS, which is a false negative\n%s", output)
	}

	if s.verdict != "" && code != s.verdict.ExitCode() {
		t.Fatalf("exit = %d, want %d (%s)\nstdout:\n%s\nstderr:\n%s",
			code, s.verdict.ExitCode(), s.verdict, output, stderrText)
	}

	for _, id := range s.mustFire {
		if !strings.Contains(output, string(id)) {
			t.Errorf("%s was not reported\n%s", id, output)
		}
	}
	for _, id := range s.mustNotFire {
		if strings.Contains(output, string(id)) {
			t.Errorf("%s was reported but should not have been\n%s", id, output)
		}
	}
}
