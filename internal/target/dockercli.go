package target

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/redact"
)

// dockerBinary is the CLI this package drives. It is never passed to a shell
// and no argument is ever interpolated into a command string.
const dockerBinary = "docker"

const (
	maxDockerOutputBytes = 8 << 20
	maxDockerErrorBytes  = 64 << 10
)

// dockerCLI talks to the daemon by executing the docker binary.
//
// This is the only file that knows Docker's output format. Everything above it
// works against ContainerState, so a change in the CLI contract has exactly one
// place to be fixed. See docs/adr/0014-docker-via-cli.md.
type dockerCLI struct{}

func newDockerCLI() dockerClient { return dockerCLI{} }

// inspectPayload is the subset of `docker inspect` output that is parsed.
// Docker spells the binding field "HostIp", which is why the tag is explicit.
type inspectPayload struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	State struct {
		Running   bool `json:"Running"`
		ExitCode  int  `json:"ExitCode"`
		OOMKilled bool `json:"OOMKilled"`
		Pid       int  `json:"Pid"`
	} `json:"State"`
	Config struct {
		Image string `json:"Image"`
	} `json:"Config"`
	NetworkSettings struct {
		Ports map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		} `json:"Ports"`
	} `json:"NetworkSettings"`
}

func (dockerCLI) Ping(ctx context.Context) error {
	// Asking for the server version specifically is what distinguishes a
	// missing daemon from a missing CLI; the client version answers even when
	// nothing is listening.
	_, err := runDocker(ctx, "version", "--format", "{{.Server.Version}}")
	return err
}

func (dockerCLI) Inspect(ctx context.Context, container string) (ContainerState, error) {
	out, err := runDocker(ctx, "inspect", "--type", "container", "--", container)
	if err != nil {
		return ContainerState{}, err
	}

	var payloads []inspectPayload
	if err := json.Unmarshal(out, &payloads); err != nil {
		return ContainerState{}, fmt.Errorf("parsing docker inspect output: %w", err)
	}
	if len(payloads) == 0 {
		return ContainerState{}, fmt.Errorf("%w: %s", ErrContainerNotRunning, container)
	}
	p := payloads[0]
	if !ValidContainerID(p.ID) {
		return ContainerState{}, fmt.Errorf("parsing docker inspect output: invalid container ID")
	}

	ports := map[string][]PortBinding{}
	for port, bindings := range p.NetworkSettings.Ports {
		for _, b := range bindings {
			ports[port] = append(ports[port], PortBinding{HostIP: b.HostIP, HostPort: b.HostPort})
		}
	}

	return ContainerState{
		ID:        p.ID,
		Name:      strings.TrimPrefix(p.Name, "/"),
		Image:     p.Config.Image,
		Running:   p.State.Running,
		ExitCode:  p.State.ExitCode,
		OOMKilled: p.State.OOMKilled,
		PID:       p.State.Pid,
		Ports:     ports,
	}, nil
}

func (dockerCLI) Kill(ctx context.Context, container string, sig Signal) error {
	_, err := runDocker(ctx, "kill", "--signal="+string(sig), "--", container)
	return err
}

func (dockerCLI) Wait(ctx context.Context, container string) (int, error) {
	out, err := runDocker(ctx, "wait", "--", container)
	if err != nil {
		return 0, err
	}

	line := strings.TrimSpace(string(out))
	code, err := strconv.Atoi(line)
	if err != nil {
		return 0, fmt.Errorf("unexpected docker wait output %q", line)
	}
	return code, nil
}

func (dockerCLI) FollowLogs(ctx context.Context, container string, sink LogSink, maxBytes int64) (func() error, error) {
	streamCtx, cancel := context.WithCancel(ctx)

	// --tail 0 starts at the moment of attachment. Replaying history would put
	// output from before the run on the timeline at the wrong offsets, which is
	// worse than having none.
	cmd := exec.CommandContext(streamCtx, dockerBinary, // #nosec G204 -- fixed binary, validated container ref
		"logs", "--follow", "--tail", "0", "--", container)
	stdout := NewLogWriter("stdout", maxBytes, sink)
	stderr := NewLogWriter("stderr", maxBytes, sink)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, dockerError("logs", err, nil)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = cmd.Wait()
		flushLogWriter(stdout)
		flushLogWriter(stderr)
	}()

	var once sync.Once
	return func() error {
		once.Do(func() {
			cancel()
			<-done
		})
		return nil
	}, nil
}

func runDocker(ctx context.Context, args ...string) ([]byte, error) {
	stdout := newCappedBuffer(maxDockerOutputBytes)
	stderr := newCappedBuffer(maxDockerErrorBytes)

	cmd := exec.CommandContext(ctx, dockerBinary, args...) // #nosec G204 -- fixed binary, validated container ref
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Run(); err != nil {
		return nil, dockerError(args[0], err, stderr.Bytes())
	}
	if stdout.Truncated() {
		return nil, fmt.Errorf("docker %s output exceeded the %d-byte limit", args[0], maxDockerOutputBytes)
	}
	return stdout.Bytes(), nil
}

type cappedBuffer struct {
	bytes.Buffer
	max       int
	truncated bool
}

func newCappedBuffer(maxBytes int) *cappedBuffer {
	return &cappedBuffer{max: maxBytes}
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	written := len(p)
	remaining := b.max - b.Len()
	if remaining <= 0 {
		b.truncated = b.truncated || written > 0
		return written, nil
	}
	if len(p) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	_, _ = b.Buffer.Write(p)
	return written, nil
}

func (b *cappedBuffer) Truncated() bool { return b.truncated }

// dockerError turns an exec failure into something a user can act on.
//
// The distinction that matters is "the tool could not run" versus "the service
// is broken": every error here maps to exit code 4, and a stack trace or a
// misleading verdict would be worse than either.
func dockerError(subcommand string, err error, stderr []byte) error {
	if errors.Is(err, exec.ErrNotFound) {
		return fmt.Errorf("%w: the docker CLI is not on PATH; install Docker or use --pid or `-- <command>` instead",
			ErrDockerUnavailable)
	}

	detail := strings.TrimSpace(string(stderr))
	if detail == "" {
		detail = err.Error()
	}
	detail = redact.Message(detail)

	if isDaemonUnreachable(detail) {
		return fmt.Errorf("%w: %s", ErrDockerUnavailable, detail)
	}
	if isNoSuchContainer(detail) {
		return fmt.Errorf("%w: %s", ErrContainerNotRunning, detail)
	}
	return fmt.Errorf("docker %s failed: %s", subcommand, detail)
}

func isDaemonUnreachable(detail string) bool {
	lower := strings.ToLower(detail)
	return strings.Contains(lower, "cannot connect to the docker daemon") ||
		strings.Contains(lower, "is the docker daemon running") ||
		strings.Contains(lower, "permission denied while trying to connect")
}

func isNoSuchContainer(detail string) bool {
	lower := strings.ToLower(detail)
	return strings.Contains(lower, "no such container") ||
		strings.Contains(lower, "no such object")
}
