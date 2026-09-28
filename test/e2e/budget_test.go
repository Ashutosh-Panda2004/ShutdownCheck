//go:build !windows

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Ashutosh-Panda2004/ShutdownCheck/pkg/schema"
)

// Budgets from spec section 19. They are asserted here rather than measured by
// hand at release time, because a size or latency regression arrives one commit
// at a time and is invisible unless something is watching.
const (
	maxBinaryBytes = 15 << 20

	// Generous against the ~10ms a Go binary actually needs. The budget exists
	// to catch someone adding expensive package-level initialisation, not to
	// police the scheduling noise of a shared CI runner.
	maxStartup = 200 * time.Millisecond
)

// releaseBinary builds with the flags the release pipeline uses, so the size
// asserted is the size that ships.
func releaseBinary(t *testing.T) string {
	t.Helper()

	out := filepath.Join(t.TempDir(), "shutdowncheck")
	cmd := exec.Command("go", "build",
		"-trimpath", "-ldflags", "-s -w",
		"-o", out, "../../cmd/shutdowncheck")
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		t.Fatalf("release build failed: %v", err)
	}
	return out
}

func TestBinaryFitsItsSizeBudget(t *testing.T) {
	info, err := os.Stat(releaseBinary(t))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	t.Logf("release binary is %.2f MB", float64(info.Size())/(1<<20))
	if info.Size() > maxBinaryBytes {
		t.Errorf("binary is %d bytes, budget is %d", info.Size(), maxBinaryBytes)
	}
}

// A tool that runs in every CI job pays its startup cost on every job. This
// measures the fast path, since `version` does no work beyond initialising the
// program and printing a line.
func TestStartupIsFast(t *testing.T) {
	binary := releaseBinary(t)

	// Warm the page cache and let any on-access scanner do its first pass, or
	// the first measurement describes the filesystem rather than the program.
	for range 3 {
		if err := exec.Command(binary, "version").Run(); err != nil {
			t.Fatalf("version: %v", err)
		}
	}

	const samples = 10
	times := make([]time.Duration, 0, samples)
	for range samples {
		start := time.Now()
		if err := exec.Command(binary, "version").Run(); err != nil {
			t.Fatalf("version: %v", err)
		}
		times = append(times, time.Since(start))
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })

	// The median rather than the mean: one descheduled run should not fail the
	// build, but a genuine regression moves the whole distribution.
	median := times[len(times)/2]
	t.Logf("startup: best %s, median %s, worst %s", times[0], median, times[len(times)-1])

	if median > maxStartup {
		t.Errorf("median startup %s exceeds the %s budget", median, maxStartup)
	}
}

// Spec section 19 requires that repeated runs of the same fixture reach the
// same verdict. A verification tool that is only usually right is not one
// anybody can gate a merge on, and an intermittent verdict is indistinguishable
// from a broken service to whoever is looking at the failure.
//
// The count is modest by default because each iteration terminates a real
// process; -stability raises it to the fifty the spec asks for.
func TestVerdictIsStableAcrossRepeatedRuns(t *testing.T) {
	runs := 6
	if *stability {
		runs = 50
	}

	cases := map[string]struct {
		mode    string
		profile string
		want    int
	}{
		"correct service passes every time": {"correct", "standalone", schema.ExitPass},
		"broken service fails every time":   {"instant-close", "kubernetes", schema.ExitFail},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			seen := map[int]int{}

			for i := range runs {
				args := toolArgs(t, tc.mode, "--profile", tc.profile, "--no-timeline")
				got := runTool(args...)
				seen[got.code]++

				if got.code != tc.want {
					t.Errorf("run %d: exit = %d, want %d\n%s", i+1, got.code, tc.want, got.stdout)
				}
			}

			if len(seen) > 1 {
				t.Errorf("the verdict was not stable across %d runs: %v", runs, seen)
			}
		})
	}
}

// A run against a target that never exits must end by itself rather than
// hanging the pipeline until the CI job is killed.
func TestRunHasAWallClockCeiling(t *testing.T) {
	got := runTool(append(
		toolArgs(t, "ignore-signal", "--no-timeline"),
		"--timeout", "3s",
	)...)

	if got.code == schema.ExitPass {
		t.Fatalf("a target that ignores signals must not pass\n%s", got.stdout)
	}
	// Either the ordinary SIGKILL escalation ended it, or the ceiling did. Both
	// are acceptable; hanging is not, and reaching here at all proves it did not.
	if got.code == schema.ExitTarget && !strings.Contains(got.stderr, "ceiling") {
		t.Logf("ended via target error rather than the ceiling: %s", got.stderr)
	}
}
