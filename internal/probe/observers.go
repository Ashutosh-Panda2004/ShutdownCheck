package probe

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/shutdowncheck/shutdowncheck/internal/redact"
	"github.com/shutdowncheck/shutdowncheck/internal/timeline"
)

// Readiness polls the target's readiness endpoint.
//
// It deliberately keeps its own client with keep-alives disabled rather than
// sharing the load prober's pool. Sharing would let a saturated connection pool
// delay readiness polls, and the exact instant readiness flips is the entire
// measurement for stage S2.
type Readiness struct {
	client *http.Client
	url    string
	now    func() time.Duration
}

// NewReadiness builds a readiness prober.
func NewReadiness(rawURL string, timeout time.Duration, insecure bool, now func() time.Duration) *Readiness {
	if now == nil {
		now = func() time.Duration { return 0 }
	}
	if timeout <= 0 {
		timeout = 2 * time.Second
	}

	transport := &http.Transport{
		DisableKeepAlives: true,
		ForceAttemptHTTP2: false,
		TLSNextProto:      map[string]func(string, *tls.Conn) http.RoundTripper{},
		// #nosec G402 -- opt-in only, via the explicit --insecure flag.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure, MinVersion: tls.VersionTLS12},
		DialContext:     (&net.Dialer{Timeout: timeout}).DialContext,
	}

	return &Readiness{
		client: &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: noRedirect},
		url:    rawURL,
		now:    now,
	}
}

// Poll performs one readiness check.
//
// Only a 2xx counts as healthy. A connection refused after the process has gone
// is correctly reported as unhealthy rather than as an error, because that is
// exactly what a load balancer would conclude.
func (r *Readiness) Poll(ctx context.Context) timeline.Event {
	start := r.now()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return r.event(start, timeline.ReadinessEvent{Outcome: timeline.OutcomeOther})
	}

	resp, err := r.client.Do(req)
	if err != nil {
		outcome, _ := Classify(err)
		return r.event(start, timeline.ReadinessEvent{Outcome: outcome})
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()

	return r.event(start, timeline.ReadinessEvent{
		Status:  resp.StatusCode,
		Healthy: resp.StatusCode >= 200 && resp.StatusCode < 300,
		Outcome: ClassifyStatus(resp.StatusCode),
	})
}

func (r *Readiness) event(start time.Duration, e timeline.ReadinessEvent) timeline.Event {
	done := r.now()
	e.Latency = done - start
	return timeline.ReadinessAt(done, e)
}

// Close releases the prober's transport.
func (r *Readiness) Close() {
	if t, ok := r.client.Transport.(*http.Transport); ok {
		t.CloseIdleConnections()
	}
}

// Listener probes whether the target socket still accepts connections, using a
// bare TCP dial and no HTTP.
//
// This is the only way to tell "the listener is closed" from "the listener is
// open but the application never answers". The first is a refused connection
// and fails fast; the second hangs the caller for its full timeout and is a
// materially worse defect.
type Listener struct {
	addr    string
	dialer  *net.Dialer
	timeout time.Duration
	now     func() time.Duration
}

// NewListener builds a listener prober for a host:port address.
func NewListener(addr string, timeout time.Duration, now func() time.Duration) *Listener {
	if now == nil {
		now = func() time.Duration { return 0 }
	}
	if timeout <= 0 {
		timeout = time.Second
	}
	return &Listener{addr: addr, dialer: &net.Dialer{Timeout: timeout}, timeout: timeout, now: now}
}

// Probe performs one dial and closes it immediately.
func (l *Listener) Probe(ctx context.Context) timeline.Event {
	start := l.now()

	conn, err := l.dialer.DialContext(ctx, "tcp", l.addr)
	if err != nil {
		outcome, _ := Classify(err)
		done := l.now()
		return timeline.ListenerAt(done, timeline.ListenerEvent{
			Accepting: false,
			Outcome:   outcome,
			Latency:   done - start,
		})
	}
	_ = conn.Close()

	done := l.now()
	return timeline.ListenerAt(done, timeline.ListenerEvent{
		Accepting: true,
		Outcome:   timeline.OutcomeOK,
		Latency:   done - start,
	})
}

// AddrFromURL derives the host:port to dial from a target URL, applying the
// scheme's default port when none is given.
func AddrFromURL(rawURL string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if parsed.Host == "" {
		return "", &url.Error{Op: "parse", URL: redact.URL(rawURL), Err: errNoHost}
	}

	if parsed.Port() != "" {
		return parsed.Host, nil
	}

	port := 80
	if parsed.Scheme == "https" {
		port = 443
	}
	return net.JoinHostPort(parsed.Hostname(), strconv.Itoa(port)), nil
}

type constError string

func (e constError) Error() string { return string(e) }

const errNoHost = constError("url has no host")
