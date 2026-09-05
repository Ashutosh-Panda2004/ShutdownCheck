package target

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shutdowncheck/shutdowncheck/internal/clock"
	"github.com/shutdowncheck/shutdowncheck/internal/testutil"
)

func TestValidatePID(t *testing.T) {
	self := os.Getpid()

	cases := map[string]struct {
		pid     int
		wantErr bool
	}{
		"normal":   {self + 1, false},
		"zero":     {0, true},
		"negative": {-5, true},
		"init":     {1, true},
		"self":     {self, true},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := ValidatePID(tc.pid, self, false)
			if tc.wantErr != (err != nil) {
				t.Fatalf("ValidatePID(%d) error = %v, wantErr %v", tc.pid, err, tc.wantErr)
			}

			if tc.wantErr {
				var unsafe *UnsafeTargetError
				if !errors.As(err, &unsafe) {
					t.Errorf("error should be an UnsafeTargetError, got %T", err)
				}
			}
		})
	}
}

// PID 1 is init on a host and the service itself inside a container, so the
// refusal has to be overridable. Signalling shutdowncheck is never coherent:
// the measurement would destroy the process taking it, so there is no reading
// under which that is what someone meant.
func TestAllowUnsafePIDPermitsInitButNeverSelf(t *testing.T) {
	self := os.Getpid()

	if err := ValidatePID(1, self, true); err != nil {
		t.Errorf("PID 1 should be allowed with the opt-in: %v", err)
	}
	if err := ValidatePID(self, self, true); err == nil {
		t.Error("the opt-in must not permit signalling shutdowncheck itself")
	}
	for _, pid := range []int{0, -1} {
		if err := ValidatePID(pid, self, true); err == nil {
			t.Errorf("PID %d is not a process id and the opt-in must not permit it", pid)
		}
	}
}

// The refusal has to name the way out, or it is just an obstacle.
func TestInitRefusalNamesTheOptIn(t *testing.T) {
	err := ValidatePID(1, os.Getpid(), false)
	if err == nil {
		t.Fatal("PID 1 should be refused by default")
	}
	if !strings.Contains(err.Error(), "--allow-unsafe-pid") {
		t.Errorf("the refusal should name the flag that permits it: %v", err)
	}
}

func TestSignalValidity(t *testing.T) {
	for _, s := range []Signal{SIGTERM, SIGINT, SIGQUIT, SIGKILL} {
		if !s.Valid() {
			t.Errorf("%q should be valid", s)
		}
	}
	if Signal("HUP").Valid() {
		t.Error("HUP is not supported and should not report itself valid")
	}

	for _, s := range []Signal{SIGTERM, SIGINT, SIGQUIT} {
		if !s.Graceful() {
			t.Errorf("%q should count as graceful", s)
		}
	}
	if SIGKILL.Graceful() {
		t.Error("SIGKILL must never count as graceful")
	}
}

// An attached process cannot be waited on, so its exit code is genuinely
// unavailable. Rendering that as "exit 0" would let a clean-exit check pass on
// evidence the tool never had.
func TestExitStatusString(t *testing.T) {
	cases := map[string]struct {
		status ExitStatus
		want   string
	}{
		"unknown":  {ExitStatus{}, "unknown"},
		"clean":    {ExitStatus{Known: true, Code: 0}, "exit 0"},
		"nonzero":  {ExitStatus{Known: true, Code: 3}, "exit 3"},
		"signaled": {ExitStatus{Known: true, Signaled: true, TerminatedBy: SIGKILL}, "killed by SIGKILL"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := tc.status.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReadyCheckConfigured(t *testing.T) {
	if (ReadyCheck{}).Configured() {
		t.Error("an empty check has nothing to wait for")
	}
	if !(ReadyCheck{URL: "http://x/"}).Configured() {
		t.Error("a URL check is configured")
	}
	if !(ReadyCheck{Addr: "localhost:1"}).Configured() {
		t.Error("an address check is configured")
	}
}

func TestWaitReadyNoCheckReturnsImmediately(t *testing.T) {
	if err := WaitReady(context.Background(), ReadyCheck{}, clock.System(), nil); err != nil {
		t.Fatalf("WaitReady with no check: %v", err)
	}
}

func TestWaitReadyHTTPSucceedsOnceServing(t *testing.T) {
	testutil.NoLeaks(t)

	var ready bool
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if !ready {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	go func() {
		time.Sleep(80 * time.Millisecond)
		mu.Lock()
		ready = true
		mu.Unlock()
	}()

	err := WaitReady(context.Background(), ReadyCheck{
		URL:      srv.URL,
		Timeout:  3 * time.Second,
		Interval: 10 * time.Millisecond,
	}, clock.System(), nil)
	if err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
}

// A 404 still proves the server is up; only a 5xx suggests it is still starting.
func TestWaitReadyAcceptsClientErrors(t *testing.T) {
	testutil.NoLeaks(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	err := WaitReady(context.Background(), ReadyCheck{
		URL:      srv.URL,
		Timeout:  time.Second,
		Interval: 10 * time.Millisecond,
	}, clock.System(), nil)
	if err != nil {
		t.Fatalf("a 404 should count as up, got: %v", err)
	}
}

func TestWaitReadyTCP(t *testing.T) {
	testutil.NoLeaks(t)

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	err := WaitReady(context.Background(), ReadyCheck{
		Addr:     addr,
		Timeout:  time.Second,
		Interval: 10 * time.Millisecond,
	}, clock.System(), nil)
	if err != nil {
		t.Fatalf("WaitReady over TCP: %v", err)
	}
}

func TestWaitReadyTimesOut(t *testing.T) {
	testutil.NoLeaks(t)

	err := WaitReady(context.Background(), ReadyCheck{
		Addr:     "127.0.0.1:1",
		Timeout:  120 * time.Millisecond,
		Interval: 10 * time.Millisecond,
	}, clock.System(), nil)

	if !IsReadyTimeout(err) {
		t.Fatalf("error = %v, want a readiness timeout", err)
	}
}

func TestReadyAttemptTimeoutDoesNotExceedOverallBudget(t *testing.T) {
	probe := newReadyProbe(ReadyCheck{Addr: "127.0.0.1:1"}, 10*time.Millisecond, 120*time.Millisecond)
	defer probe.close()

	if probe.dialer.Timeout != 120*time.Millisecond || probe.client.Timeout != 120*time.Millisecond {
		t.Fatalf("attempt timeouts = %s/%s, want 120ms", probe.dialer.Timeout, probe.client.Timeout)
	}
}

func TestReadyTimeoutDoesNotLeakURLCredentials(t *testing.T) {
	const secret = "ready-secret-must-not-appear"
	err := (&ErrReadyTimeout{
		Check:   ReadyCheck{URL: "https://user:" + secret + "@example.test/ready?token=" + secret},
		Waited:  time.Second,
		LastErr: errors.New("Get https://example.test/ready?api_key=" + secret + ": refused"),
	}).Error()

	if strings.Contains(err, secret) {
		t.Fatalf("readiness error leaked credentials: %s", err)
	}
}

// A target that crashes on boot must fail immediately with the reason, not burn
// the whole timeout and then report a misleading "not ready".
func TestWaitReadyFailsFastWhenTargetDies(t *testing.T) {
	testutil.NoLeaks(t)

	start := time.Now()
	err := WaitReady(context.Background(), ReadyCheck{
		Addr:     "127.0.0.1:1",
		Timeout:  10 * time.Second,
		Interval: 10 * time.Millisecond,
	}, clock.System(), func() (bool, error) { return false, nil })

	var died *ErrDiedDuringStartup
	if !errors.As(err, &died) {
		t.Fatalf("error = %v, want ErrDiedDuringStartup", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %v to notice the target had died; it should fail fast", elapsed)
	}
}

func TestWaitReadyPropagatesLivenessErrors(t *testing.T) {
	testutil.NoLeaks(t)

	want := errors.New("liveness unavailable")
	err := WaitReady(context.Background(), ReadyCheck{
		Addr:     "127.0.0.1:1",
		Timeout:  10 * time.Second,
		Interval: 10 * time.Millisecond,
	}, clock.System(), func() (bool, error) { return false, want })

	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want wrapped liveness error", err)
	}
}

func TestWaitReadyHonoursContext(t *testing.T) {
	testutil.NoLeaks(t)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := WaitReady(ctx, ReadyCheck{
		Addr:     "127.0.0.1:1",
		Timeout:  10 * time.Second,
		Interval: 10 * time.Millisecond,
	}, clock.System(), nil)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

type captured struct {
	mu    sync.Mutex
	lines []string
}

func (c *captured) sink(stream, line string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, stream+": "+line)
}

func (c *captured) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.lines...)
}

func TestLogWriterSplitsLines(t *testing.T) {
	c := &captured{}
	w := NewLogWriter("stdout", 0, c.sink)

	_, _ = w.Write([]byte("first\nsec"))
	_, _ = w.Write([]byte("ond\r\nthird\n"))

	got := c.all()
	want := []string{"stdout: first", "stdout: second", "stdout: third"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLogWriterFlushEmitsTrailingPartialLine(t *testing.T) {
	c := &captured{}
	w := NewLogWriter("stderr", 0, c.sink)

	_, _ = w.Write([]byte("no trailing newline"))
	if len(c.all()) != 0 {
		t.Fatal("a partial line should not be emitted until flushed")
	}

	w.(interface{ Flush() }).Flush()
	if got := c.all(); len(got) != 1 || got[0] != "stderr: no trailing newline" {
		t.Fatalf("after flush got %v", got)
	}
}

// SECURITY.md promises captured target logs are scrubbed, so a service that
// logs its own request URLs cannot leak a token into the report.
func TestLogWriterRedacts(t *testing.T) {
	c := &captured{}
	w := NewLogWriter("stdout", 0, c.sink)

	_, _ = w.Write([]byte(`handled "http://svc/x?token=supersecret" ok` + "\n"))

	got := c.all()
	if len(got) != 1 {
		t.Fatalf("got %v", got)
	}
	if strings.Contains(got[0], "supersecret") {
		t.Fatalf("captured log leaked a secret: %q", got[0])
	}
}

func TestLogWriterCapsTotalBytes(t *testing.T) {
	c := &captured{}
	w := NewLogWriter("stdout", 20, c.sink)

	for range 20 {
		_, _ = w.Write([]byte("0123456789\n"))
	}

	got := c.all()
	var truncationNotices int
	for _, line := range got {
		if strings.Contains(line, "log capture truncated") {
			truncationNotices++
		}
	}
	if truncationNotices != 1 {
		t.Fatalf("expected exactly one truncation notice, got %d in %d lines", truncationNotices, len(got))
	}
	if len(got) > 5 {
		t.Errorf("capture kept %d lines despite a 20 byte cap", len(got))
	}
}

func TestLogWriterCapsLineLength(t *testing.T) {
	c := &captured{}
	w := NewLogWriter("stdout", 0, c.sink)

	_, _ = w.Write([]byte(strings.Repeat("x", maxLogLineBytes*2) + "\n"))

	got := c.all()
	if len(got) != 1 {
		t.Fatalf("got %d lines, want 1", len(got))
	}
	if len(got[0]) > maxLogLineBytes+64 {
		t.Errorf("line was not truncated: %d bytes", len(got[0]))
	}
	if !strings.Contains(got[0], "line truncated") {
		t.Error("truncation should be marked in the line")
	}
}

func TestLogWriterCapsLineCount(t *testing.T) {
	c := &captured{}
	w := NewLogWriter("stdout", int64(maxLogLines*4), c.sink)

	for range maxLogLines + 10 {
		_, _ = w.Write([]byte("x\n"))
	}

	got := c.all()
	if len(got) != maxLogLines+1 {
		t.Fatalf("captured %d lines, want %d data lines plus one notice", len(got), maxLogLines)
	}
	if !strings.Contains(got[len(got)-1], "log capture truncated") {
		t.Fatalf("final line is not a truncation notice: %q", got[len(got)-1])
	}
}

func TestLogWriterWithoutSinkIsANoOp(t *testing.T) {
	w := NewLogWriter("stdout", 0, nil)
	if n, err := w.Write([]byte("anything\n")); err != nil || n != 9 {
		t.Fatalf("Write = %d, %v", n, err)
	}
}

func TestNewCommandRejectsEmptyArgv(t *testing.T) {
	for _, argv := range [][]string{nil, {}, {"   "}} {
		if _, err := NewCommand(CommandOptions{Argv: argv}); err == nil {
			t.Errorf("NewCommand(%v) should have failed", argv)
		}
	}
}

func TestNewProcessRejectsUnsafePIDs(t *testing.T) {
	for _, pid := range []int{0, 1, -1, os.Getpid()} {
		if _, err := NewProcess(ProcessOptions{PID: pid}); err == nil {
			t.Errorf("NewProcess(pid=%d) should have been refused", pid)
		}
	}
}

// ADR-0008: Windows must fail loudly rather than substitute a hard kill for a
// graceful one, because that would make every Windows verdict a lie.
func TestWindowsRefusesProcessTargetsHonestly(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-specific behaviour")
	}

	_, err := NewCommand(CommandOptions{Argv: []string{"cmd", "/c", "echo hi"}})
	if !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("error = %v, want ErrUnsupportedPlatform", err)
	}
	if !strings.Contains(err.Error(), "--docker") {
		t.Errorf("the error should point at the supported alternative, got: %v", err)
	}
}
