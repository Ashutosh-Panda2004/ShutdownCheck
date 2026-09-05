package probe

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/shutdowncheck/shutdowncheck/internal/redact"
	"github.com/shutdowncheck/shutdowncheck/internal/testutil"
	"github.com/shutdowncheck/shutdowncheck/internal/timeline"
)

// collector gathers connection events emitted by a prober.
type collector struct {
	mu     sync.Mutex
	events []timeline.Event
}

func (c *collector) sink(e timeline.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}

func (c *collector) snapshot() []timeline.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]timeline.Event(nil), c.events...)
}

func (c *collector) ofPhase(p timeline.ConnPhase) []timeline.ConnectionEvent {
	var out []timeline.ConnectionEvent
	for _, e := range c.snapshot() {
		if e.Kind == timeline.KindConnection && e.Connection.Phase == p {
			out = append(out, *e.Connection)
		}
	}
	return out
}

func newTestProber(t *testing.T, c *collector, opts Options) *HTTP {
	t.Helper()

	start := time.Now()
	opts.Now = func() time.Duration { return time.Since(start) }
	opts.Sink = c.sink
	if opts.Timeout == 0 {
		opts.Timeout = 2 * time.Second
	}

	p := NewHTTP(opts)
	t.Cleanup(p.Close)
	return p
}

func TestDoSuccess(t *testing.T) {
	testutil.NoLeaks(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	c := &collector{}
	p := newTestProber(t, c, Options{KeepAlive: true})

	got := p.Do(context.Background(), Request{Method: "GET", URL: srv.URL})
	if got.Outcome != timeline.OutcomeOK {
		t.Fatalf("outcome = %q (%s), want ok", got.Outcome, got.Error)
	}
	if got.Status != http.StatusOK {
		t.Errorf("status = %d, want 200", got.Status)
	}
	if got.ConnID == 0 {
		t.Error("no connection id was recorded")
	}
	if got.ConnReused {
		t.Error("the first request cannot have reused a connection")
	}
	if len(c.ofPhase(timeline.ConnOpen)) != 1 {
		t.Errorf("expected exactly one connection-open event, got %d", len(c.ofPhase(timeline.ConnOpen)))
	}
}

func TestNewHTTPUsesBoundedDefaultTimeout(t *testing.T) {
	prober := NewHTTP(Options{})
	defer prober.Close()
	if prober.client.Timeout != DefaultTimeout {
		t.Errorf("client timeout = %s, want %s", prober.client.Timeout, DefaultTimeout)
	}
}

func TestDoNonSuccessStatusIsAFailure(t *testing.T) {
	testutil.NoLeaks(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	p := newTestProber(t, &collector{}, Options{KeepAlive: true})

	got := p.Do(context.Background(), Request{Method: "GET", URL: srv.URL})
	if got.Outcome != timeline.OutcomeHTTPError {
		t.Fatalf("outcome = %q, want http_error", got.Outcome)
	}
	if got.Status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", got.Status)
	}
}

func TestDoTimeoutWhenServerHangs(t *testing.T) {
	testutil.NoLeaks(t)

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	defer func() {
		close(release)
		srv.Close()
	}()

	p := newTestProber(t, &collector{}, Options{KeepAlive: true, Timeout: 150 * time.Millisecond})

	got := p.Do(context.Background(), Request{Method: "GET", URL: srv.URL})
	if got.Outcome != timeline.OutcomeTimeout {
		t.Fatalf("outcome = %q (%s), want timeout", got.Outcome, got.Error)
	}
}

func TestDoRefusedWhenListenerClosed(t *testing.T) {
	testutil.NoLeaks(t)

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	p := newTestProber(t, &collector{}, Options{KeepAlive: true, Timeout: time.Second})

	got := p.Do(context.Background(), Request{Method: "GET", URL: url})
	if got.Outcome != timeline.OutcomeRefused {
		t.Fatalf("outcome = %q (%s), want refused", got.Outcome, got.Error)
	}
}

// A response whose body dies partway through must never be counted as served.
// This is precisely how a mid-response reset during shutdown presents itself,
// and reporting it as a success would hide the headline defect the tool exists
// to find.
func TestDoTruncatedResponseIsNotSuccess(t *testing.T) {
	testutil.NoLeaks(t)

	addr := rawServer(t, func(conn net.Conn) {
		_, _ = bufio.NewReader(conn).ReadString('\n')
		// Promise far more than is delivered, then destroy the socket.
		_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 4096\r\n\r\npartial")
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0)
		}
		_ = conn.Close()
	})

	c := &collector{}
	p := newTestProber(t, c, Options{KeepAlive: true, Timeout: 2 * time.Second})

	got := p.Do(context.Background(), Request{Method: "GET", URL: "http://" + addr + "/"})
	if got.Outcome == timeline.OutcomeOK {
		t.Fatal("a truncated response was reported as a success")
	}
	if got.Outcome != timeline.OutcomeReset && got.Outcome != timeline.OutcomeEOF {
		t.Errorf("outcome = %q (%s), want reset or eof", got.Outcome, got.Error)
	}
}

func TestDoRecordsServerConnectionClose(t *testing.T) {
	testutil.NoLeaks(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Connection", "close")
		fmt.Fprint(w, "bye")
	}))
	defer srv.Close()

	c := &collector{}
	p := newTestProber(t, c, Options{KeepAlive: true})

	got := p.Do(context.Background(), Request{Method: "GET", URL: srv.URL})
	if got.Outcome != timeline.OutcomeOK {
		t.Fatalf("outcome = %q, want ok", got.Outcome)
	}
	if !got.ServerClose {
		t.Fatal("the server sent `Connection: close` but it was not recorded")
	}

	waitFor(t, func() bool { return len(c.ofPhase(timeline.ConnClose)) > 0 })
	closes := c.ofPhase(timeline.ConnClose)
	if !closes[0].ServerClose {
		t.Error("the close event should record that the server asked for the close")
	}
}

func TestKeepAliveReusesConnection(t *testing.T) {
	testutil.NoLeaks(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	c := &collector{}
	p := newTestProber(t, c, Options{KeepAlive: true})

	first := p.Do(context.Background(), Request{Method: "GET", URL: srv.URL})
	second := p.Do(context.Background(), Request{Method: "GET", URL: srv.URL})

	if first.ConnID != second.ConnID {
		t.Errorf("connection ids differ (%d, %d); the connection should have been reused", first.ConnID, second.ConnID)
	}
	if second.ConnReused != true {
		t.Error("the second request should report a reused connection")
	}
	if got := len(c.ofPhase(timeline.ConnOpen)); got != 1 {
		t.Errorf("opened %d connections, want 1", got)
	}
	if got := len(c.ofPhase(timeline.ConnReuse)); got != 1 {
		t.Errorf("recorded %d reuse events, want 1", got)
	}
}

func TestKeepAliveDisabledOpensFreshConnections(t *testing.T) {
	testutil.NoLeaks(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	c := &collector{}
	p := newTestProber(t, c, Options{KeepAlive: false})

	first := p.Do(context.Background(), Request{Method: "GET", URL: srv.URL})
	second := p.Do(context.Background(), Request{Method: "GET", URL: srv.URL})

	if first.ConnID == second.ConnID {
		t.Error("connections were reused despite keep-alive being disabled")
	}
	if got := len(c.ofPhase(timeline.ConnOpen)); got != 2 {
		t.Errorf("opened %d connections, want 2", got)
	}
}

func TestDoSendsBodyAndHeaders(t *testing.T) {
	testutil.NoLeaks(t)

	var (
		gotBody   string
		gotHeader string
		gotMethod string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotHeader, gotMethod = string(b), r.Header.Get("X-Test"), r.Method
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	p := newTestProber(t, &collector{}, Options{KeepAlive: true})

	got := p.Do(context.Background(), Request{
		Method:  "POST",
		URL:     srv.URL,
		Headers: map[string]string{"X-Test": "value"},
		Body:    []byte(`{"a":1}`),
	})

	if got.Outcome != timeline.OutcomeOK {
		t.Fatalf("outcome = %q, want ok", got.Outcome)
	}
	if gotMethod != "POST" || gotBody != `{"a":1}` || gotHeader != "value" {
		t.Errorf("server saw method=%q body=%q header=%q", gotMethod, gotBody, gotHeader)
	}
}

// Following a redirect would silently measure a different endpoint than the one
// under test.
func TestDoDoesNotFollowRedirects(t *testing.T) {
	testutil.NoLeaks(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusTeapot)
	}))
	defer srv.Close()

	p := newTestProber(t, &collector{}, Options{KeepAlive: true})

	got := p.Do(context.Background(), Request{Method: "GET", URL: srv.URL + "/"})
	if got.Status != http.StatusFound {
		t.Fatalf("status = %d, want 302 (redirects must not be followed)", got.Status)
	}
}

func TestHTTPResponseBodyLimit(t *testing.T) {
	for name, tc := range map[string]struct {
		body        string
		wantOutcome timeline.Outcome
		wantError   bool
	}{
		"exact limit succeeds": {body: "1234", wantOutcome: timeline.OutcomeOK},
		"one byte over fails":  {body: "12345", wantOutcome: timeline.OutcomeOther, wantError: true},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			p := newTestProber(t, &collector{}, Options{KeepAlive: true, MaxBodyBytes: 4})
			got := p.Do(context.Background(), Request{Method: http.MethodGet, URL: srv.URL})
			if got.Outcome != tc.wantOutcome {
				t.Errorf("outcome = %q, want %q (error %q)", got.Outcome, tc.wantOutcome, got.Error)
			}
			if tc.wantError != (got.Error != "") {
				t.Errorf("error = %q, wantError %v", got.Error, tc.wantError)
			}
		})
	}
}

func TestDoInvalidURL(t *testing.T) {
	p := newTestProber(t, &collector{}, Options{KeepAlive: true})

	got := p.Do(context.Background(), Request{Method: "GET", URL: "://nonsense"})
	if got.Outcome == timeline.OutcomeOK {
		t.Fatal("an unparseable URL must not produce a success")
	}
}

func TestDoRejectsInvalidHTTPMetadata(t *testing.T) {
	p := newTestProber(t, &collector{}, Options{KeepAlive: true})

	for name, request := range map[string]Request{
		"method":       {Method: "BAD METHOD", URL: "http://example.test/"},
		"header name":  {Method: "GET", URL: "http://example.test/", Headers: map[string]string{"Bad Header": "value"}},
		"header value": {Method: "GET", URL: "http://example.test/", Headers: map[string]string{"X-Test": "value\r\ninjected"}},
	} {
		t.Run(name, func(t *testing.T) {
			got := p.Do(context.Background(), request)
			if got.Outcome == timeline.OutcomeOK || got.Error == "" {
				t.Fatalf("invalid HTTP metadata produced %+v", got)
			}
		})
	}
}

func TestClassifyTermination(t *testing.T) {
	cases := map[string]struct {
		err  error
		want timeline.ConnTermination
	}{
		"clean eof":       {io.EOF, timeline.TermFIN},
		"unexpected eof":  {io.ErrUnexpectedEOF, timeline.TermFIN},
		"reset":           {syscall.ECONNRESET, timeline.TermRST},
		"windows reset":   {syscall.Errno(wsaeConnReset), timeline.TermRST},
		"wrapped reset":   {&net.OpError{Op: "read", Err: syscall.ECONNRESET}, timeline.TermRST},
		"client closed":   {nil, timeline.TermUnknown},
		"something else":  {errors.New("boom"), timeline.TermUnknown},
		"timeout on read": {&timeoutError{}, timeline.TermTimeout},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := classifyTermination(tc.err); got != tc.want {
				t.Errorf("classifyTermination(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestClassify(t *testing.T) {
	cases := map[string]struct {
		err  error
		want timeline.Outcome
	}{
		"nil":             {nil, timeline.OutcomeOK},
		"refused":         {&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, timeline.OutcomeRefused},
		"windows refused": {syscall.Errno(wsaeConnRefused), timeline.OutcomeRefused},
		"reset":           {&net.OpError{Op: "read", Err: syscall.ECONNRESET}, timeline.OutcomeReset},
		"eof":             {io.EOF, timeline.OutcomeEOF},
		"deadline":        {context.DeadlineExceeded, timeline.OutcomeTimeout},
		"net timeout":     {&timeoutError{}, timeline.OutcomeTimeout},
		"cancelled":       {context.Canceled, timeline.OutcomeAbandoned},
		"dns":             {&net.DNSError{Err: "no such host"}, timeline.OutcomeDNSError},
		"unknown":         {errors.New("boom"), timeline.OutcomeOther},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, _ := Classify(tc.err)
			if got != tc.want {
				t.Errorf("Classify(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// A token in a query string reaches the report through the error string alone,
// so classification must scrub what it returns.
func TestClassifyRedactsURLsInDetail(t *testing.T) {
	err := fmt.Errorf(`Get "http://host/x?token=supersecret": dial tcp: refused`)

	_, detail := Classify(err)
	if strings.Contains(detail, "supersecret") {
		t.Fatalf("classification detail leaked a secret: %q", detail)
	}
	if !strings.Contains(detail, redact.Placeholder) {
		t.Errorf("detail should mark the redaction, got %q", detail)
	}
}

func TestRemoteAddrHandlesNil(t *testing.T) {
	if got := remoteAddr(nil); got != "" {
		t.Errorf("remoteAddr(nil) = %q, want empty", got)
	}
}

func TestTrackedFromRejectsUnknownConn(t *testing.T) {
	if _, ok := trackedFrom(nil); ok {
		t.Error("trackedFrom(nil) should not report success")
	}
}

func TestClassifyStatus(t *testing.T) {
	for _, status := range []int{200, 201, 204, 299} {
		if got := ClassifyStatus(status); got != timeline.OutcomeOK {
			t.Errorf("ClassifyStatus(%d) = %q, want ok", status, got)
		}
	}
	for _, status := range []int{100, 301, 400, 500, 503} {
		if got := ClassifyStatus(status); got != timeline.OutcomeHTTPError {
			t.Errorf("ClassifyStatus(%d) = %q, want http_error", status, got)
		}
	}
}

func TestListenerProbe(t *testing.T) {
	testutil.NoLeaks(t)

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr, err := AddrFromURL(srv.URL)
	if err != nil {
		t.Fatalf("AddrFromURL: %v", err)
	}

	start := time.Now()
	now := func() time.Duration { return time.Since(start) }
	l := NewListener(addr, time.Second, now)

	open := l.Poll(context.Background())
	if !open.Listener.Accepting {
		t.Fatalf("listener should be accepting, got outcome %q", open.Listener.Outcome)
	}

	srv.Close()

	closed := l.Poll(context.Background())
	if closed.Listener.Accepting {
		t.Fatal("listener should not be accepting after the server closed")
	}
	if closed.Listener.Outcome != timeline.OutcomeRefused {
		t.Errorf("outcome = %q, want refused", closed.Listener.Outcome)
	}
}

func TestReadinessProbe(t *testing.T) {
	testutil.NoLeaks(t)

	var healthy = true
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if !healthy {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	start := time.Now()
	r := NewReadiness(srv.URL, time.Second, false, func() time.Duration { return time.Since(start) })
	defer r.Close()

	if got := r.Poll(context.Background()); !got.Readiness.Healthy {
		t.Fatalf("expected healthy, got status %d outcome %q", got.Readiness.Status, got.Readiness.Outcome)
	}

	mu.Lock()
	healthy = false
	mu.Unlock()

	if got := r.Poll(context.Background()); got.Readiness.Healthy {
		t.Fatal("expected unhealthy after the endpoint started failing")
	}

	srv.Close()
	got := r.Poll(context.Background())
	if got.Readiness.Healthy {
		t.Fatal("a refused readiness probe must count as unhealthy")
	}
}

func TestAddrFromURL(t *testing.T) {
	cases := map[string]string{
		"http://localhost:8080/x":  "localhost:8080",
		"http://localhost/x":       "localhost:80",
		"https://example.com/y":    "example.com:443",
		"https://example.com:8443": "example.com:8443",
	}
	for raw, want := range cases {
		got, err := AddrFromURL(raw)
		if err != nil {
			t.Errorf("AddrFromURL(%q): %v", raw, err)
			continue
		}
		if got != want {
			t.Errorf("AddrFromURL(%q) = %q, want %q", raw, got, want)
		}
	}

	for _, bad := range []string{"://nope", "not-a-url"} {
		if _, err := AddrFromURL(bad); err == nil {
			t.Errorf("AddrFromURL(%q) should have failed", bad)
		}
	}
}

// rawServer runs a TCP server that lets a test control the exact bytes and the
// exact manner of the close, which httptest cannot express.
func rawServer(t *testing.T, handle func(net.Conn)) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			handle(conn)
		}
	}()

	t.Cleanup(func() {
		_ = ln.Close()
		<-done
	})
	return ln.Addr().String()
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within the deadline")
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }
