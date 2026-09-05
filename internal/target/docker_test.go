package target

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shutdowncheck/shutdowncheck/internal/analyze"
)

// dockerTarget is driven through a fake daemon so that every path above the CLI
// seam is exercised on machines with no Docker installed. Without this the
// target would only ever be tested in CI, on one platform, against whichever
// daemon version happened to be there.

type fakeDocker struct {
	mu sync.Mutex

	pingErr    error
	inspectErr error
	killErr    error
	waitErr    error
	logsErr    error

	state      ContainerState
	waitCode   int
	killed     []Signal
	logsStops  int
	inspects   int
	inspectRef string
	killRef    string
	waitRef    string
	logsRef    string

	// holdExit keeps the container notionally running until it is closed, so
	// the transition from alive to exited can be observed deliberately.
	holdExit chan struct{}
}

func (f *fakeDocker) Ping(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pingErr
}

func (f *fakeDocker) Inspect(_ context.Context, container string) (ContainerState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.inspects++
	f.inspectRef = container
	if f.inspectErr != nil {
		return ContainerState{}, f.inspectErr
	}
	return f.state, nil
}

func (f *fakeDocker) Kill(_ context.Context, container string, sig Signal) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.killErr != nil {
		return f.killErr
	}
	f.killRef = container
	f.killed = append(f.killed, sig)
	return nil
}

func (f *fakeDocker) Wait(ctx context.Context, container string) (int, error) {
	f.mu.Lock()
	f.waitRef = container
	hold := f.holdExit
	err := f.waitErr
	code := f.waitCode
	f.mu.Unlock()

	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	if err != nil {
		return 0, err
	}
	return code, nil
}

func (f *fakeDocker) FollowLogs(_ context.Context, container string, _ LogSink, _ int64) (func() error, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.logsErr != nil {
		return nil, f.logsErr
	}
	f.logsRef = container
	return func() error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.logsStops++
		return nil
	}, nil
}

func (f *fakeDocker) signalsSent() []Signal {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Signal(nil), f.killed...)
}

func runningContainer() ContainerState {
	return ContainerState{
		ID: strings.Repeat("a", 64), Name: "api", Image: "api:latest",
		Running: true, PID: 4242,
		Ports: map[string][]PortBinding{
			"8080/tcp": {{HostIP: "0.0.0.0", HostPort: "32768"}},
		},
	}
}

func TestDockerPinsOperationsToInspectedContainerID(t *testing.T) {
	hold := make(chan struct{})
	client := &fakeDocker{state: runningContainer(), holdExit: hold}
	tgt := newFakeDockerTarget(t, client, DockerOptions{LogSink: func(string, string) {}})

	if err := tgt.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := tgt.Signal(SIGTERM); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	if err := tgt.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	want := client.state.ID
	if client.inspectRef != "api" {
		t.Errorf("initial inspect used %q, want original name api", client.inspectRef)
	}
	for operation, got := range map[string]string{
		"kill": client.killRef, "wait": client.waitRef, "logs": client.logsRef,
	} {
		if got != want {
			t.Errorf("%s used %q, want immutable ID %q", operation, got, want)
		}
	}
}

func TestDockerCloseJoinsExitWatcher(t *testing.T) {
	client := &fakeDocker{state: runningContainer(), holdExit: make(chan struct{})}
	targetInterface := newFakeDockerTarget(t, client, DockerOptions{})
	tgt := targetInterface.(*dockerTarget)

	if err := tgt.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := tgt.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-tgt.exited:
	default:
		t.Fatal("Close returned before the exit watcher stopped")
	}
}

func newFakeDockerTarget(t *testing.T, client *fakeDocker, opts DockerOptions) Target {
	t.Helper()

	opts.client = client
	if opts.Container == "" {
		opts.Container = "api"
	}

	tgt, err := NewDocker(opts)
	if err != nil {
		t.Fatalf("NewDocker: %v", err)
	}
	return tgt
}

// A container reference reaches argv, so anything the docker CLI would read as
// a flag has to be refused before it gets there.
func TestDockerRejectsReferencesThatCouldBeReadAsFlags(t *testing.T) {
	rejected := map[string]string{
		"leading dash":       "-rm",
		"long flag":          "--volume=/:/host",
		"empty":              "",
		"whitespace only":    "   ",
		"embedded space":     "api container",
		"shell metachar":     "api;rm -rf /",
		"pipe":               "api|sh",
		"command sub":        "api$(whoami)",
		"backtick":           "api`id`",
		"path traversal":     "../../etc/passwd",
		"newline injection":  "api\nkill",
		"leading dot":        ".api",
		"leading underscore": "_api",
	}

	for name, ref := range rejected {
		t.Run(name, func(t *testing.T) {
			if _, err := NewDocker(DockerOptions{Container: ref, client: &fakeDocker{}}); err == nil {
				t.Fatalf("accepted %q", ref)
			}
		})
	}
}

func TestDockerAcceptsRealNamesAndIds(t *testing.T) {
	accepted := []string{
		"api",
		"my-api",
		"my_api_1",
		"compose-project_api_1",
		"api.staging",
		"3f4a9c1b2d5e",
		"3f4a9c1b2d5e8f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b7c6d5e4f3a",
	}

	for _, ref := range accepted {
		if _, err := NewDocker(DockerOptions{Container: ref, client: &fakeDocker{}}); err != nil {
			t.Errorf("rejected %q: %v", ref, err)
		}
	}
}

func TestDockerReportsItselfAsADockerTarget(t *testing.T) {
	client := &fakeDocker{state: runningContainer()}
	tgt := newFakeDockerTarget(t, client, DockerOptions{Container: "api"})

	if err := tgt.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	desc := tgt.Describe()
	if desc.Kind != analyze.TargetDocker {
		t.Errorf("kind = %q, want %q", desc.Kind, analyze.TargetDocker)
	}
	if desc.Label != "docker(api)" {
		t.Errorf("label = %q", desc.Label)
	}
	if desc.PID != 4242 {
		t.Errorf("pid = %d, want the container's pid for context", desc.PID)
	}
}

// A missing daemon is the tool failing to run, not the service failing to shut
// down, and the two must never be confused.
func TestDockerUnavailableDaemonIsDistinctFromADefect(t *testing.T) {
	client := &fakeDocker{pingErr: errors.New("cannot connect to the Docker daemon")}
	tgt := newFakeDockerTarget(t, client, DockerOptions{})

	err := tgt.Start(context.Background())
	if !errors.Is(err, ErrDockerUnavailable) {
		t.Fatalf("err = %v, want ErrDockerUnavailable", err)
	}
}

func TestDockerErrorsDoNotLeakCredentials(t *testing.T) {
	const secret = "docker-secret-must-not-appear"
	err := dockerError("inspect", errors.New("exit status 1"), []byte(
		`cannot connect to "https://user:`+secret+`@docker.internal/?token=`+secret+`"`,
	))

	if strings.Contains(err.Error(), secret) {
		t.Fatalf("docker error leaked credentials: %v", err)
	}
}

func TestDockerOutputBufferIsBounded(t *testing.T) {
	buffer := newCappedBuffer(4)
	payload := []byte("123456")

	n, err := buffer.Write(payload)
	if err != nil || n != len(payload) {
		t.Fatalf("Write = %d, %v; want %d, nil", n, err, len(payload))
	}
	if got := buffer.String(); got != "1234" {
		t.Errorf("retained output = %q, want %q", got, "1234")
	}
	if !buffer.Truncated() {
		t.Error("buffer did not report truncation")
	}
}

func TestDockerRefusesAStoppedContainer(t *testing.T) {
	client := &fakeDocker{state: ContainerState{Name: "api", Running: false}}
	tgt := newFakeDockerTarget(t, client, DockerOptions{})

	err := tgt.Start(context.Background())
	if !errors.Is(err, ErrContainerNotRunning) {
		t.Fatalf("err = %v, want ErrContainerNotRunning", err)
	}
}

func TestDockerStartIsNotRepeatable(t *testing.T) {
	client := &fakeDocker{state: runningContainer()}
	tgt := newFakeDockerTarget(t, client, DockerOptions{})

	if err := tgt.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := tgt.Start(context.Background()); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("second Start = %v, want ErrAlreadyStarted", err)
	}
}

func TestDockerSignalsReachTheDaemon(t *testing.T) {
	client := &fakeDocker{state: runningContainer()}
	tgt := newFakeDockerTarget(t, client, DockerOptions{})

	if err := tgt.Signal(SIGTERM); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	if err := tgt.Signal(SIGKILL); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	if err := tgt.Signal("HUP"); err == nil {
		t.Error("an unknown signal should be refused rather than passed to the CLI")
	}

	if got := client.signalsSent(); len(got) != 2 || got[0] != SIGTERM || got[1] != SIGKILL {
		t.Errorf("signals = %v", got)
	}
}

// Misreading a kill as a deliberate non-zero exit would raise SC013 when the
// real finding is SC002, so the 128+n mapping is pinned.
func TestDockerExitCodesDistinguishSignalsFromDeliberateExits(t *testing.T) {
	cases := map[string]struct {
		code     int
		signaled bool
		by       Signal
	}{
		"clean exit":        {0, false, ""},
		"deliberate error":  {1, false, ""},
		"terminated":        {143, true, SIGTERM},
		"killed":            {137, true, SIGKILL},
		"interrupted":       {130, true, SIGINT},
		"quit":              {131, true, SIGQUIT},
		"not a signal code": {129, false, ""},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			client := &fakeDocker{state: runningContainer(), waitCode: tc.code}
			tgt := newFakeDockerTarget(t, client, DockerOptions{})

			status, err := tgt.Wait(context.Background())
			if err != nil {
				t.Fatalf("Wait: %v", err)
			}
			if !status.Known {
				t.Fatal("a container exit code is always observable")
			}
			if status.Code != tc.code {
				t.Errorf("code = %d, want %d", status.Code, tc.code)
			}
			if status.Signaled != tc.signaled {
				t.Errorf("signaled = %v, want %v", status.Signaled, tc.signaled)
			}
			if status.TerminatedBy != tc.by {
				t.Errorf("terminated by = %q, want %q", status.TerminatedBy, tc.by)
			}
		})
	}
}

// An OOM kill during drain is indistinguishable from a slow drain unless it is
// said out loud.
func TestDockerOOMKillReachesTheTimeline(t *testing.T) {
	state := runningContainer()
	state.OOMKilled = true

	var lines []string
	var mu sync.Mutex
	client := &fakeDocker{state: state, waitCode: 137}
	tgt := newFakeDockerTarget(t, client, DockerOptions{
		LogSink: func(stream, line string) {
			mu.Lock()
			defer mu.Unlock()
			lines = append(lines, stream+": "+line)
		},
	})

	if _, err := tgt.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "stderr: [shutdowncheck] container was OOM-killed") {
		t.Errorf("the OOM kill was not surfaced: %q", joined)
	}
}

// The runner asks Alive every 10ms while waiting for exit. If that reached the
// daemon it would spawn roughly a thousand processes per run and quantise the
// measured shutdown duration to process-spawn latency, which is the one number
// this tool exists to report accurately.
func TestDockerLivenessDoesNotPollTheDaemon(t *testing.T) {
	client := &fakeDocker{state: runningContainer(), holdExit: make(chan struct{})}
	tgt := newFakeDockerTarget(t, client, DockerOptions{})
	t.Cleanup(func() { _ = tgt.Close() })

	if err := tgt.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	client.mu.Lock()
	afterStart := client.inspects
	client.mu.Unlock()

	for range 500 {
		alive, err := tgt.Alive()
		if err != nil {
			t.Fatalf("Alive: %v", err)
		}
		if !alive {
			t.Fatal("the container is still running")
		}
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	if client.inspects != afterStart {
		t.Errorf("liveness cost %d daemon calls; it should cost none",
			client.inspects-afterStart)
	}
}

func TestDockerObservesExitThroughTheWatcher(t *testing.T) {
	hold := make(chan struct{})
	client := &fakeDocker{state: runningContainer(), waitCode: 143, holdExit: hold}
	tgt := newFakeDockerTarget(t, client, DockerOptions{})
	t.Cleanup(func() { _ = tgt.Close() })

	if err := tgt.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if alive, err := tgt.Alive(); err != nil || !alive {
		t.Fatalf("alive = %v, err = %v; the container has not stopped yet", alive, err)
	}

	close(hold)

	status, err := tgt.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !status.Signaled || status.TerminatedBy != SIGTERM {
		t.Errorf("status = %+v, want a SIGTERM death", status)
	}

	alive, err := tgt.Alive()
	if err != nil {
		t.Fatalf("Alive: %v", err)
	}
	if alive {
		t.Error("the container exited but is still reported as alive")
	}
}

// Waiting must be abandonable, or a container that never stops would hang the
// run past every deadline the runner enforces.
func TestDockerWaitRespectsContextCancellation(t *testing.T) {
	client := &fakeDocker{state: runningContainer(), holdExit: make(chan struct{})}
	tgt := newFakeDockerTarget(t, client, DockerOptions{})
	t.Cleanup(func() { _ = tgt.Close() })

	if err := tgt.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := tgt.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline to be honoured", err)
	}
}

func TestDockerDefaultGracePeriod(t *testing.T) {
	client := &fakeDocker{state: runningContainer()}

	tgt := newFakeDockerTarget(t, client, DockerOptions{})
	if got := tgt.DefaultGracePeriod(); got != 10*time.Second {
		t.Errorf("grace = %s, want the 10s docker stop default", got)
	}

	tgt = newFakeDockerTarget(t, client, DockerOptions{GracePeriod: 3 * time.Second})
	if got := tgt.DefaultGracePeriod(); got != 3*time.Second {
		t.Errorf("grace = %s, want the explicit value", got)
	}
}

// Attaching to someone's running container and then stopping it would be a
// surprising side effect of a diagnostic tool.
func TestDockerCloseStopsStreamingWithoutStoppingTheContainer(t *testing.T) {
	client := &fakeDocker{state: runningContainer()}
	tgt := newFakeDockerTarget(t, client, DockerOptions{
		LogSink: func(string, string) {},
	})

	if err := tgt.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for range 3 {
		if err := tgt.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	if client.logsStops != 1 {
		t.Errorf("log streaming stopped %d times, want exactly one", client.logsStops)
	}
	if len(client.killed) != 0 {
		t.Errorf("Close signalled the container: %v", client.killed)
	}
}

func TestDockerCloseIsSafeBeforeStart(t *testing.T) {
	tgt := newFakeDockerTarget(t, &fakeDocker{}, DockerOptions{})
	if err := tgt.Close(); err != nil {
		t.Errorf("Close before Start: %v", err)
	}
}

func TestResolveContainerPort(t *testing.T) {
	multi := map[string][]PortBinding{
		"8080/tcp": {{HostIP: "0.0.0.0", HostPort: "32768"}},
		"9090/tcp": {{HostIP: "0.0.0.0", HostPort: "32769"}},
	}

	cases := map[string]struct {
		state   ContainerState
		port    string
		want    string
		wantErr bool
	}{
		"single published port is unambiguous": {
			state: runningContainer(), want: "127.0.0.1:32768",
		},
		"explicit container port": {
			state: ContainerState{Name: "api", Ports: multi}, port: "9090", want: "127.0.0.1:32769",
		},
		"explicit port with protocol": {
			state: ContainerState{Name: "api", Ports: multi}, port: "9090/tcp", want: "127.0.0.1:32769",
		},
		"several ports need a choice": {
			state: ContainerState{Name: "api", Ports: multi}, wantErr: true,
		},
		"no published ports": {
			state: ContainerState{Name: "api"}, wantErr: true,
		},
		"unpublished port requested": {
			state: ContainerState{Name: "api", Ports: multi}, port: "7000", wantErr: true,
		},
		"udp port is not an HTTP target": {
			state: ContainerState{Name: "api", Ports: map[string][]PortBinding{
				"5353/udp": {{HostIP: "0.0.0.0", HostPort: "5353"}},
			}}, wantErr: true,
		},
		"invalid explicit port": {
			state: ContainerState{Name: "api", Ports: multi}, port: "not-a-port", wantErr: true,
		},
		"invalid host binding": {
			state: ContainerState{Name: "api", Ports: map[string][]PortBinding{
				"8080/tcp": {{HostIP: "not an ip", HostPort: "1234"}},
			}}, wantErr: true,
		},
		"invalid host port": {
			state: ContainerState{Name: "api", Ports: map[string][]PortBinding{
				"8080/tcp": {{HostIP: "127.0.0.1", HostPort: "70000"}},
			}}, wantErr: true,
		},
		"wildcard bind becomes loopback": {
			state: ContainerState{Name: "api", Ports: map[string][]PortBinding{
				"8080/tcp": {{HostIP: "", HostPort: "1234"}},
			}}, want: "127.0.0.1:1234",
		},
		"ipv6 host is bracketed": {
			state: ContainerState{Name: "api", Ports: map[string][]PortBinding{
				"8080/tcp": {{HostIP: "fd00::1", HostPort: "1234"}},
			}}, want: "[fd00::1]:1234",
		},
		"ipv6 wildcard becomes loopback": {
			state: ContainerState{Name: "api", Ports: map[string][]PortBinding{
				"8080/tcp": {{HostIP: "::", HostPort: "1234"}},
			}}, want: "127.0.0.1:1234",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := ResolveContainerPort(tc.state, tc.port)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("got %q, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveContainerPort: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// Ambiguity has to name the options, or the user has to go and run docker
// inspect themselves to find out what to pass.
func TestAmbiguousPortErrorNamesTheCandidates(t *testing.T) {
	state := ContainerState{Name: "api", Ports: map[string][]PortBinding{
		"9090/tcp": {{HostPort: "32769"}},
		"8080/tcp": {{HostPort: "32768"}},
	}}

	_, err := ResolveContainerPort(state, "")
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	for _, want := range []string{"8080/tcp", "9090/tcp", "--container-port"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error is missing %q: %s", want, msg)
		}
	}
	// Listed in a stable order so the message does not change between runs.
	if strings.Index(msg, "8080/tcp") > strings.Index(msg, "9090/tcp") {
		t.Errorf("candidates are not sorted: %s", msg)
	}
}
