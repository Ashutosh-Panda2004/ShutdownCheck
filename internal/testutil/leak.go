// Package testutil holds helpers shared by tests. Nothing here ships in the
// binary.
package testutil

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// NoLeaks fails the test if goroutines outlive it.
//
// Leaked goroutines in this codebase are not a tidiness issue: a probe or
// ticker that survives its run keeps writing into a recorder that analysis has
// already snapshotted, and a CI runner that accumulates them across a soak test
// eventually stops behaving like the machine the measurements assume.
//
// Call it as the first statement of a test. It registers through t.Cleanup
// rather than returning a function to defer, because deferred calls run before
// cleanups and would observe servers the test has not shut down yet.
func NoLeaks(t *testing.T) {
	t.Helper()
	before := goroutineNames()

	t.Cleanup(func() {
		// Goroutines unwind asynchronously, so a single sample would be flaky.
		deadline := time.Now().Add(2 * time.Second)
		for {
			leaked := diff(before, goroutineNames())
			if len(leaked) == 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("%d goroutine(s) leaked:\n%s", len(leaked), strings.Join(leaked, "\n\n"))
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}

func goroutineNames() map[string]string {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]

	out := map[string]string{}
	for _, block := range strings.Split(string(buf), "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" || isIgnored(block) {
			continue
		}
		if id, _, ok := strings.Cut(block, " "); ok {
			out[id] = block
		}
	}
	return out
}

// isIgnored skips goroutines owned by the runtime and the test harness, which
// come and go independently of the code under test.
func isIgnored(block string) bool {
	for _, marker := range []string{
		"testing.tRunner",
		"testing.(*M).Run",
		"runtime.goexit",
		"created by runtime",
		"os/signal.loop",
		"runtime.gcBgMarkWorker",
		"testutil.goroutineNames",
	} {
		if strings.Contains(block, marker) {
			return true
		}
	}
	return false
}

func diff(before, after map[string]string) []string {
	var leaked []string
	for id, stack := range after {
		if _, existed := before[id]; !existed {
			leaked = append(leaked, stack)
		}
	}
	return leaked
}
