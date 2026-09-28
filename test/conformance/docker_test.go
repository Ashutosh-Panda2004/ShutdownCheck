//go:build !windows

package conformance

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/cli"
)

// The Docker target has to reach the same verdicts as the process target, or
// "it works in your container too" is an untested claim. The same scenario
// table drives both, and both are checked by the same assertions.

const dockerImage = "shutdowncheck-conformance:test"

func TestDockerConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("the docker conformance run builds an image and starts containers; run without -short")
	}

	if reason := dockerUsable(); reason != "" {
		// Keyed on an explicit variable rather than CI, because Docker is
		// genuinely absent on some hosted runners. Where we have promised a
		// daemon, a skip would mean the Docker target ships untested, which is
		// exactly what this suite exists to prevent.
		if os.Getenv("SHUTDOWNCHECK_REQUIRE_DOCKER") != "" {
			t.Fatalf("docker was required but is unusable, so the docker target is untested: %s", reason)
		}
		t.Skip(reason)
	}

	buildImage(t)

	for _, s := range scenarios {
		if _, excluded := dockerExcludedModes[s.mode]; excluded {
			continue
		}
		t.Run(s.mode+"/"+s.profile, func(t *testing.T) {
			runDockerScenario(t, s)
		})
	}
}

func dockerUsable() string {
	cmd := exec.Command("docker", "version", "--format", "{{.Server.Version}}")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "docker is unavailable: " + strings.TrimSpace(string(out))
	}
	return ""
}

func buildImage(t *testing.T) {
	t.Helper()

	// The build context is the module root, because the image compiles the
	// conformance server from the tree under test rather than a published one.
	cmd := exec.Command("docker", "build",
		"-f", "test/conformance/docker/Dockerfile",
		"-t", dockerImage, ".")
	cmd.Dir = "../.."

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the conformance image failed: %v\n%s", err, out)
	}
}

func runDockerScenario(t *testing.T, s scenario) {
	t.Helper()

	name := fmt.Sprintf("shutdowncheck-conf-%d", time.Now().UnixNano())

	// Published on an ephemeral loopback port so parallel runs and busy CI
	// machines cannot collide, and so the run never listens on a public
	// interface.
	start := exec.Command("docker", "run", "--detach",
		"--name", name,
		"--publish", "127.0.0.1::8080",
		dockerImage,
		"-addr", "0.0.0.0:8080", "-mode", s.mode,
	)
	out, err := start.CombinedOutput()
	if err != nil {
		t.Fatalf("starting the container failed: %v\n%s", err, out)
	}

	t.Cleanup(func() {
		// A container left running poisons every job that follows it on the
		// same runner, so removal is forced and unconditional.
		_ = exec.Command("docker", "rm", "--force", name).Run()
	})

	args := []string{
		"run",
		"--docker", name,
		// Path-only URLs exercise port resolution: the host port is ephemeral
		// and is not known until the container is running.
		"--url", "/work",
		"--readiness-url", "/readyz",
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

	var stdout, stderr bytes.Buffer
	code := cli.Main(args, &stdout, &stderr)
	assertScenario(t, s, code, stdout.String(), stderr.String())
}
