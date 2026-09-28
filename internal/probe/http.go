package probe

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"

	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/redact"
	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/timeline"
)

// DefaultMaxBodyBytes caps how much of a response body is read. Bodies are
// discarded, never retained, but they must be read for a keep-alive connection
// to become reusable.
const (
	DefaultTimeout      = 10 * time.Second
	DefaultMaxBodyBytes = 1 << 20
)

// ErrResponseBodyTooLarge means the complete response was not observed.
var ErrResponseBodyTooLarge = errors.New("response body exceeds the configured limit")

// Request is one resolved request definition.
type Request struct {
	Name    string
	Method  string
	URL     string
	Headers map[string]string
	Body    []byte
}

// Attempt is the outcome of a single request.
type Attempt struct {
	Status      int
	Outcome     timeline.Outcome
	Error       string
	ConnID      uint64
	ConnReused  bool
	ServerClose bool
}

// Options configures the HTTP prober.
type Options struct {
	Timeout      time.Duration
	KeepAlive    bool
	Insecure     bool
	MaxConns     int
	MaxBodyBytes int64

	// Now returns the current offset from run start; connection events are
	// stamped with it.
	Now func() time.Duration
	// Sink receives connection lifecycle events.
	Sink Sink
}

// HTTP issues instrumented requests against the target.
type HTTP struct {
	client    *http.Client
	transport *http.Transport
	tracker   *connTracker
	maxBody   int64
}

// NewHTTP builds a prober.
func NewHTTP(opts Options) *HTTP {
	if opts.Now == nil {
		opts.Now = func() time.Duration { return 0 }
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.MaxConns <= 0 {
		opts.MaxConns = 512
	}

	tracker := newConnTracker(opts.Sink, opts.Now)
	dialer := &net.Dialer{Timeout: opts.Timeout, KeepAlive: 30 * time.Second}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return tracker.wrap(c), nil
		},
		DisableKeepAlives:   !opts.KeepAlive,
		MaxIdleConns:        opts.MaxConns,
		MaxIdleConnsPerHost: opts.MaxConns,
		MaxConnsPerHost:     0,
		IdleConnTimeout:     90 * time.Second,

		// v1 verifies HTTP/1.1 only. HTTP/2 multiplexes many requests onto one
		// connection, which changes what "in flight" and "connection closed"
		// mean; GOAWAY handling is deliberately deferred rather than
		// approximated. An empty TLSNextProto disables the h2 upgrade.
		ForceAttemptHTTP2: false,
		TLSNextProto:      map[string]func(string, *tls.Conn) http.RoundTripper{},

		// #nosec G402 -- verification is on unless the operator passed --insecure,
		// which prints a warning and is recorded in the report.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: opts.Insecure, MinVersion: tls.VersionTLS12},
	}

	return &HTTP{
		client:    &http.Client{Transport: transport, Timeout: opts.Timeout, CheckRedirect: noRedirect},
		transport: transport,
		tracker:   tracker,
		maxBody:   opts.MaxBodyBytes,
	}
}

// noRedirect stops the client following redirects: a 302 is a response the
// target produced, and following it would measure a different endpoint.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// connState collects what the httptrace hooks observe. The hooks can run on the
// transport's goroutine, so access is guarded.
type connState struct {
	mu           sync.Mutex
	conn         *trackedConn
	id           uint64
	reused       bool
	tlsStart     time.Duration
	tlsHandshake time.Duration
}

// Do performs one request and classifies the result.
func (h *HTTP) Do(ctx context.Context, req Request) Attempt {
	state := &connState{}

	httpReq, err := h.build(ctx, req, state)
	if err != nil {
		return Attempt{Outcome: timeline.OutcomeOther, Error: redact.Message(err.Error())}
	}

	resp, err := h.client.Do(httpReq)
	if err != nil {
		outcome, detail := Classify(err)
		return h.attempt(state, Attempt{Outcome: outcome, Error: detail})
	}

	if resp.Close {
		state.mu.Lock()
		conn := state.conn
		state.mu.Unlock()
		if conn != nil {
			conn.markServerClose()
		}
	}

	// A body that fails partway through is a dropped request even though the
	// status line already arrived; this is exactly how a mid-response reset
	// during shutdown presents itself.
	read, readErr := io.Copy(io.Discard, io.LimitReader(resp.Body, h.maxBody+1))
	if readErr == nil && read > h.maxBody {
		readErr = fmt.Errorf("%w of %d bytes", ErrResponseBodyTooLarge, h.maxBody)
	}
	closeErr := resp.Body.Close()
	if readErr == nil {
		readErr = closeErr
	}

	attempt := Attempt{Status: resp.StatusCode, ServerClose: resp.Close}
	if readErr != nil {
		attempt.Outcome, attempt.Error = Classify(readErr)
	} else {
		attempt.Outcome = ClassifyStatus(resp.StatusCode)
	}
	return h.attempt(state, attempt)
}

func (h *HTTP) build(ctx context.Context, req Request, state *connState) (*http.Request, error) {
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	if !ValidMethod(method) {
		return nil, fmt.Errorf("invalid HTTP method")
	}

	var body io.Reader
	if len(req.Body) > 0 {
		body = bytes.NewReader(req.Body)
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, req.URL, body)
	if err != nil {
		return nil, err
	}
	for name, value := range req.Headers {
		if !ValidHeaderName(name) || !ValidHeaderValue(value) {
			return nil, fmt.Errorf("invalid HTTP header")
		}
		httpReq.Header.Set(name, value)
	}
	if len(req.Body) > 0 {
		payload := req.Body
		httpReq.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(payload)), nil
		}
	}

	return httpReq.WithContext(httptrace.WithClientTrace(httpReq.Context(), h.trace(state))), nil
}

func (h *HTTP) trace(state *connState) *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		TLSHandshakeStart: func() {
			state.mu.Lock()
			state.tlsStart = h.tracker.now()
			state.mu.Unlock()
		},
		TLSHandshakeDone: func(tls.ConnectionState, error) {
			state.mu.Lock()
			state.tlsHandshake = h.tracker.now() - state.tlsStart
			state.mu.Unlock()
		},
		GotConn: func(info httptrace.GotConnInfo) {
			tc, ok := trackedFrom(info.Conn)
			if !ok {
				return
			}

			state.mu.Lock()
			state.conn = tc
			state.id = tc.id
			state.reused = info.Reused
			handshake := state.tlsHandshake
			state.mu.Unlock()

			phase := timeline.ConnOpen
			if info.Reused {
				phase = timeline.ConnReuse
			}
			h.tracker.emit(timeline.ConnectionAt(h.tracker.now(), timeline.ConnectionEvent{
				ID:           tc.id,
				Phase:        phase,
				RemoteAddr:   remoteAddr(info.Conn),
				Reused:       info.Reused,
				TLSHandshake: handshake,
			}))
		},
	}
}

func (h *HTTP) attempt(state *connState, a Attempt) Attempt {
	state.mu.Lock()
	defer state.mu.Unlock()

	a.ConnID = state.id
	a.ConnReused = state.reused
	return a
}

func remoteAddr(c net.Conn) string {
	if c == nil || c.RemoteAddr() == nil {
		return ""
	}
	return c.RemoteAddr().String()
}

// Close releases pooled connections, flushing their close events onto the
// timeline. Without it, connections would be reaped after analysis had already
// taken its snapshot and their termination would go unrecorded.
func (h *HTTP) Close() {
	h.transport.CloseIdleConnections()
}
