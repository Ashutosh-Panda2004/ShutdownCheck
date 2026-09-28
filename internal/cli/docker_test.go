package cli

import (
	"strings"
	"testing"

	"github.com/Ashutosh-Panda2004/ShutdownCheck/pkg/schema"
)

func TestContainerPortRequiresADockerTarget(t *testing.T) {
	code, _, stderr := execute(t, "run",
		"--url", "http://localhost:8080/", "--pid", "4242", "--container-port", "8080")

	if code != schema.ExitUsage {
		t.Fatalf("exit = %d, want usage", code)
	}
	if !strings.Contains(stderr, "--container-port") {
		t.Errorf("the error should name the offending flag: %s", stderr)
	}
}

func TestInvalidContainerReferenceIsRefused(t *testing.T) {
	// A reference starting with a dash would be read by the docker CLI as a
	// flag, which is the injection path the allowlist exists to close.
	for _, ref := range []string{"-rm", "--volume=/:/host", "api;rm -rf /", "api container"} {
		code, _, _ := execute(t, "run", "--url", "http://localhost:8080/", "--docker", ref)
		if code == schema.ExitPass || code == schema.ExitFail {
			t.Errorf("%q was accepted as a container reference", ref)
		}
	}
}

// Whether or not Docker is installed here, neither outcome is a verdict about
// a service: both mean the tool could not run, and a pipeline has to be able to
// tell that apart from a detected defect.
func TestUnusableDockerTargetIsATargetError(t *testing.T) {
	code, _, stderr := execute(t, "run",
		"--url", "http://127.0.0.1:1/", "--docker", "shutdowncheck-does-not-exist",
		"--warmup", "100ms", "--steady", "100ms", "--grace-period", "1s", "--no-color")

	if code != schema.ExitTarget {
		t.Fatalf("exit = %d, want %d (target error)\nstderr: %s", code, schema.ExitTarget, stderr)
	}
	if !strings.Contains(strings.ToLower(stderr), "docker") &&
		!strings.Contains(strings.ToLower(stderr), "container") {
		t.Errorf("the error should say what went wrong with Docker: %s", stderr)
	}
}

func TestRunHelpDocumentsDockerFlags(t *testing.T) {
	_, _, stderr := execute(t, "run", "--help")

	for _, want := range []string{"--docker", "--container-port"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("run help is missing %q", want)
		}
	}
}
