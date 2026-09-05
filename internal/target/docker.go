package target

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shutdowncheck/shutdowncheck/internal/analyze"
	"github.com/shutdowncheck/shutdowncheck/internal/clock"
)

// containerRef is the allowlist every container reference must satisfy before
// it reaches argv.
//
// The leading character rule is the load-bearing part: a reference starting
// with "-" would be parsed by the docker CLI as a flag. Docker's own naming
// rules are a subset of this, so nothing legitimate is rejected.
var containerRef = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
var containerID = regexp.MustCompile(`^[a-f0-9]{64}$`)

// ValidContainerRef reports whether a container reference is safe to pass to
// the docker CLI.
//
// It is exported so that configuration validation can reject a bad reference
// with a file and line number rather than at run time. The rule lives here,
// next to the code that builds the argv, because a second copy elsewhere is a
// second copy that can drift.
func ValidContainerRef(s string) bool { return containerRef.MatchString(s) }

// ValidContainerID reports whether id is a full immutable Docker container ID.
func ValidContainerID(id string) bool { return containerID.MatchString(id) }

// ValidContainerPort reports whether value names a TCP container port.
func ValidContainerPort(value string) bool {
	_, err := normalizeContainerPort(value)
	return err == nil
}

// Errors that mean the tool could not run, as distinct from a defect it found.
var (
	// ErrDockerUnavailable reports that the docker CLI or daemon is unreachable.
	ErrDockerUnavailable = errors.New("docker is unavailable")

	// ErrContainerNotRunning reports a container that exists but is stopped, or
	// does not exist at all.
	ErrContainerNotRunning = errors.New("container is not running")

	// ErrNoPublishedPort reports that a probe URL could not be derived because
	// the container publishes no ports.
	ErrNoPublishedPort = errors.New("container publishes no ports")
)

// dockerControlTimeout bounds the one-shot daemon calls, so a wedged daemon
// fails the run with a clear message instead of hanging it forever.
const dockerControlTimeout = 15 * time.Second

// PortBinding is one host address a container port is published on.
type PortBinding struct {
	HostIP   string
	HostPort string
}

// ContainerState is the subset of `docker inspect` this tool relies on.
type ContainerState struct {
	ID       string
	Name     string
	Image    string
	Running  bool
	ExitCode int
	// OOMKilled matters during shutdown specifically: a container that is
	// killed for memory while draining looks exactly like a slow drain unless
	// the distinction is surfaced.
	OOMKilled bool
	PID       int
	// Ports maps a container port such as "8080/tcp" to its host bindings.
	Ports map[string][]PortBinding
}

// dockerClient is the entire Docker surface this package uses.
//
// Keeping it this narrow is what lets every test above this line run on a
// machine with no daemon, which matters because the alternative is a target
// implementation that is only ever exercised in CI.
type dockerClient interface {
	// Ping fails when the CLI is missing or the daemon is unreachable.
	Ping(ctx context.Context) error
	Inspect(ctx context.Context, container string) (ContainerState, error)
	Kill(ctx context.Context, container string, sig Signal) error
	// Wait blocks until the container stops and returns its exit code.
	Wait(ctx context.Context, container string) (int, error)
	// FollowLogs streams new output until the returned stop function is called.
	FollowLogs(ctx context.Context, container string, sink LogSink, maxBytes int64) (stop func() error, err error)
}

// DockerOptions configures a target attached to a running container.
type DockerOptions struct {
	Container   string
	Ready       ReadyCheck
	LogSink     LogSink
	MaxLogBytes int64
	GracePeriod time.Duration
	Clock       clock.Clock

	// client is a seam for tests; production callers leave it nil.
	client dockerClient
}

// NewDocker returns a target attached to an already-running container.
func NewDocker(opts DockerOptions) (Target, error) {
	name := strings.TrimSpace(opts.Container)
	if name == "" {
		return nil, &UnsafeTargetError{Reason: "no container was given"}
	}
	if !containerRef.MatchString(name) {
		return nil, &UnsafeTargetError{Reason: fmt.Sprintf(
			"%q is not a valid container name or id; expected letters, digits, and _ . - starting with a letter or digit", name)}
	}

	client := opts.client
	if client == nil {
		client = newDockerCLI()
	}

	return &dockerTarget{
		desc:     Descriptor{Kind: analyze.TargetDocker, Label: "docker(" + name + ")"},
		client:   client,
		name:     name,
		ready:    opts.Ready,
		sink:     opts.LogSink,
		maxBytes: opts.MaxLogBytes,
		grace:    opts.GracePeriod,
		clk:      orSystemClock(opts.Clock),
		exited:   make(chan struct{}),
	}, nil
}

type dockerTarget struct {
	desc     Descriptor
	client   dockerClient
	name     string
	ready    ReadyCheck
	sink     LogSink
	maxBytes int64
	grace    time.Duration
	clk      clock.Clock

	mu       sync.Mutex
	started  bool
	pid      int
	stopLogs func() error

	// exited closes as soon as the daemon reports the container stopped. The
	// exit is watched rather than polled because the runner asks Alive every
	// 10ms, and spawning a docker inspect at that rate would both hammer the
	// daemon and quantise the measured shutdown duration to process-spawn
	// latency — which is the one number this tool exists to report accurately.
	exited      chan struct{}
	watchCancel context.CancelFunc
	exitCode    int
	exitErr     error

	closeOnce sync.Once
	closeErr  error
}

func (t *dockerTarget) Describe() Descriptor {
	t.mu.Lock()
	defer t.mu.Unlock()

	desc := t.desc
	desc.PID = t.pid
	return desc
}

// Start attaches to the container. It never creates or starts one: the tool
// measures how a service you deployed behaves, and a container it launched
// itself would be a different container from the one in your compose file.
func (t *dockerTarget) Start(ctx context.Context) error {
	t.mu.Lock()
	if t.started {
		t.mu.Unlock()
		return ErrAlreadyStarted
	}
	t.started = true
	t.mu.Unlock()

	if err := t.client.Ping(ctx); err != nil {
		return fmt.Errorf("%w: %w", ErrDockerUnavailable, err)
	}

	state, err := t.client.Inspect(ctx, t.name)
	if err != nil {
		return err
	}
	if !state.Running {
		return fmt.Errorf("%w: %s", ErrContainerNotRunning, t.name)
	}
	if !ValidContainerID(state.ID) {
		return fmt.Errorf("docker inspect returned an invalid container ID")
	}

	t.mu.Lock()
	// The container's PID is only meaningful when the daemon shares a namespace
	// with us, so it is reported for context and never signalled.
	t.pid = state.PID
	// Every operation after this point uses the immutable ID. A mutable name can
	// be removed and reused while a run is in progress, which could otherwise
	// cause the tool to signal a different container than the one it inspected.
	t.name = state.ID
	t.mu.Unlock()

	t.watchExit()

	if t.sink != nil {
		stop, err := t.client.FollowLogs(ctx, t.name, t.sink, t.maxBytes)
		if err != nil {
			return err
		}
		t.mu.Lock()
		t.stopLogs = stop
		t.mu.Unlock()
	}

	if err := WaitReady(ctx, t.ready, t.clk, t.Alive); err != nil {
		_ = t.Close()
		return err
	}
	return nil
}

// watchExit blocks on the daemon in the background so that liveness is a channel
// read rather than a subprocess.
func (t *dockerTarget) watchExit() {
	// Deliberately not derived from the caller's context: the watcher has to
	// outlive Start, and Close is what ends it.
	ctx, cancel := context.WithCancel(context.Background())

	t.mu.Lock()
	t.watchCancel = cancel
	t.mu.Unlock()

	go func() {
		code, err := t.client.Wait(ctx, t.name)

		t.mu.Lock()
		t.exitCode, t.exitErr = code, err
		t.mu.Unlock()

		close(t.exited)
	}()
}

func (t *dockerTarget) Signal(sig Signal) error {
	if !sig.Valid() {
		return fmt.Errorf("unsupported signal %q", sig)
	}

	// Bounded so a wedged daemon cannot stall the run at the exact moment the
	// measurement starts.
	ctx, cancel := context.WithTimeout(context.Background(), dockerControlTimeout)
	defer cancel()

	return t.client.Kill(ctx, t.name, sig)
}

func (t *dockerTarget) Alive() (bool, error) {
	t.mu.Lock()
	started := t.started
	t.mu.Unlock()

	if !started {
		ctx, cancel := context.WithTimeout(context.Background(), dockerControlTimeout)
		defer cancel()

		state, err := t.client.Inspect(ctx, t.name)
		if err != nil {
			return false, err
		}
		return state.Running, nil
	}

	select {
	case <-t.exited:
		t.mu.Lock()
		defer t.mu.Unlock()
		// A failed watch is not evidence of death. Callers treat a non-nil
		// error as "unknown" and keep polling, which is what stops a daemon
		// hiccup being recorded as an exit that never happened.
		return false, t.exitErr
	default:
		return true, nil
	}
}

func (t *dockerTarget) Wait(ctx context.Context) (ExitStatus, error) {
	t.mu.Lock()
	started := t.started
	t.mu.Unlock()

	if !started {
		code, err := t.client.Wait(ctx, t.name)
		if err != nil {
			return ExitStatus{}, err
		}
		return t.finish(ctx, code), nil
	}

	select {
	case <-t.exited:
	case <-ctx.Done():
		return ExitStatus{}, ctx.Err()
	}

	t.mu.Lock()
	code, err := t.exitCode, t.exitErr
	t.mu.Unlock()
	if err != nil {
		return ExitStatus{}, err
	}
	return t.finish(ctx, code), nil
}

func (t *dockerTarget) finish(ctx context.Context, code int) ExitStatus {
	// A container killed for memory while draining looks exactly like a slow
	// drain from the outside, so the distinction goes on the timeline.
	if t.sink != nil {
		if state, err := t.client.Inspect(ctx, t.name); err == nil && state.OOMKilled {
			t.sink("stderr", "[shutdowncheck] container was OOM-killed")
		}
	}
	return exitStatusFromCode(code)
}

// exitStatusFromCode maps a container exit code onto a status.
//
// Docker reports signal deaths using the 128+n shell convention, which is all
// the interface exposes: a container that genuinely exits 137 by itself is
// indistinguishable from one that was SIGKILLed. Getting this mapping right
// matters because reading a kill as a deliberate non-zero exit would raise
// SC013 when the real finding is SC002.
func exitStatusFromCode(code int) ExitStatus {
	const signalBase = 128

	if sig, ok := signalForNumber(code - signalBase); ok {
		return ExitStatus{Known: true, Code: code, Signaled: true, TerminatedBy: sig}
	}
	return ExitStatus{Known: true, Code: code}
}

func signalForNumber(n int) (Signal, bool) {
	switch n {
	case 2:
		return SIGINT, true
	case 3:
		return SIGQUIT, true
	case 9:
		return SIGKILL, true
	case 15:
		return SIGTERM, true
	default:
		return "", false
	}
}

func (t *dockerTarget) DefaultGracePeriod() time.Duration {
	if t.grace > 0 {
		return t.grace
	}
	return analyze.DefaultGracePeriod(analyze.TargetDocker)
}

// Close stops log streaming and the exit watcher. It deliberately does not stop
// the container: shutdowncheck attached to something the user is running, and
// tearing it down afterwards would be a surprising side effect of a diagnostic
// tool.
func (t *dockerTarget) Close() error {
	t.closeOnce.Do(func() {
		t.mu.Lock()
		stop := t.stopLogs
		cancel := t.watchCancel
		t.stopLogs, t.watchCancel = nil, nil
		t.mu.Unlock()

		if cancel != nil {
			cancel()
		}
		if stop != nil {
			t.closeErr = errors.Join(t.closeErr, stop())
		}
		if cancel != nil {
			select {
			case <-t.exited:
			case <-time.After(dockerControlTimeout):
				t.closeErr = errors.Join(t.closeErr,
					fmt.Errorf("docker exit watcher did not stop within %s", dockerControlTimeout))
			}
		}
	})
	return t.closeErr
}

// InspectContainer reports a container's state, so a caller can derive a probe
// URL from the container's published ports before a run starts.
func InspectContainer(ctx context.Context, container string) (ContainerState, error) {
	name := strings.TrimSpace(container)
	if !containerRef.MatchString(name) {
		return ContainerState{}, &UnsafeTargetError{Reason: fmt.Sprintf("%q is not a valid container name or id", name)}
	}

	client := newDockerCLI()
	if err := client.Ping(ctx); err != nil {
		return ContainerState{}, fmt.Errorf("%w: %w", ErrDockerUnavailable, err)
	}
	return client.Inspect(ctx, name)
}

// ResolveContainerPort returns the host address a container port is published
// on, so a probe URL can be derived from the container rather than typed out.
//
// With no explicit port, exactly one published port is required. Guessing
// between several would mean probing an admin or metrics listener and drawing
// conclusions about the wrong socket.
func ResolveContainerPort(state ContainerState, containerPort string) (string, error) {
	if len(state.Ports) == 0 {
		return "", fmt.Errorf("%w: %s", ErrNoPublishedPort, state.Name)
	}

	if containerPort != "" {
		key, err := normalizeContainerPort(containerPort)
		if err != nil {
			return "", err
		}
		bindings, ok := state.Ports[key]
		if !ok || len(bindings) == 0 {
			return "", fmt.Errorf("%w: %s does not publish %s", ErrNoPublishedPort, state.Name, key)
		}
		return hostAddress(bindings[0])
	}

	published := make([]string, 0, len(state.Ports))
	for port, bindings := range state.Ports {
		if strings.HasSuffix(port, "/tcp") && len(bindings) > 0 {
			published = append(published, port)
		}
	}
	switch len(published) {
	case 0:
		return "", fmt.Errorf("%w: %s", ErrNoPublishedPort, state.Name)
	case 1:
		return hostAddress(state.Ports[published[0]][0])
	default:
		slices.Sort(published)
		return "", fmt.Errorf("%s publishes %s; pass --container-port to choose one",
			state.Name, strings.Join(published, ", "))
	}
}

func normalizeContainerPort(value string) (string, error) {
	port := value
	if before, protocol, found := strings.Cut(value, "/"); found {
		if protocol != "tcp" {
			return "", fmt.Errorf("container port %q must use tcp", value)
		}
		port = before
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return "", fmt.Errorf("container port %q must be between 1 and 65535", value)
	}
	return strconv.Itoa(number) + "/tcp", nil
}

// hostAddress turns a binding into something dialable. The daemon reports
// wildcard binds as 0.0.0.0 or ::, neither of which is a usable destination.
func hostAddress(b PortBinding) (string, error) {
	host := b.HostIP
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	if net.ParseIP(host) == nil {
		return "", fmt.Errorf("docker reported invalid host IP %q", host)
	}
	port, err := strconv.Atoi(b.HostPort)
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("docker reported invalid host port %q", b.HostPort)
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}
