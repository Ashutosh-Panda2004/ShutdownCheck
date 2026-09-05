package target

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/shutdowncheck/shutdowncheck/internal/clock"
	"github.com/shutdowncheck/shutdowncheck/internal/redact"
)

// Readiness polling defaults.
const (
	DefaultReadyInterval = 50 * time.Millisecond
	DefaultReadyTimeout  = 30 * time.Second
)

// ReadyCheck describes how to tell that a target has finished starting.
type ReadyCheck struct {
	// URL is polled with GET; any response below 500 counts as up.
	URL string
	// Addr is dialled over TCP; a successful connection counts as up.
	Addr string

	Timeout  time.Duration
	Interval time.Duration
	Insecure bool
}

// Configured reports whether there is anything to wait for.
func (r ReadyCheck) Configured() bool { return r.URL != "" || r.Addr != "" }

// ErrReadyTimeout reports a target that never became ready.
type ErrReadyTimeout struct {
	Check   ReadyCheck
	Waited  time.Duration
	LastErr error
}

func (e *ErrReadyTimeout) Error() string {
	where := e.Check.URL
	if where == "" {
		where = e.Check.Addr
	} else {
		where = redact.URL(where)
	}
	msg := fmt.Sprintf("target did not become ready at %s within %s", where, e.Waited)
	if e.LastErr != nil {
		msg += ": " + redact.Message(e.LastErr.Error())
	}
	return msg
}

func (e *ErrReadyTimeout) Unwrap() error { return e.LastErr }

// ErrDiedDuringStartup reports a target that exited before becoming ready.
type ErrDiedDuringStartup struct {
	Status ExitStatus
}

func (e *ErrDiedDuringStartup) Error() string {
	return "target exited during startup before it became ready (" + e.Status.String() + ")"
}

// WaitReady blocks until the target is accepting traffic.
//
// alive is consulted between polls so that a target which crashes on boot fails
// immediately with the reason, instead of burning the whole timeout and then
// reporting a misleading "not ready" error.
func WaitReady(ctx context.Context, check ReadyCheck, clk clock.Clock, alive func() (bool, error)) error {
	if !check.Configured() {
		return nil
	}
	if clk == nil {
		clk = clock.System()
	}

	interval := check.Interval
	if interval <= 0 {
		interval = DefaultReadyInterval
	}
	timeout := check.Timeout
	if timeout <= 0 {
		timeout = DefaultReadyTimeout
	}

	probe := newReadyProbe(check, interval, timeout)
	defer probe.close()

	start := clk.Now()
	var lastErr error
	for {
		if err := probe.attempt(ctx); err == nil {
			return nil
		} else {
			lastErr = err
		}

		if alive != nil {
			running, err := alive()
			if err != nil {
				return fmt.Errorf("observe target during startup: %w", err)
			}
			if !running {
				return &ErrDiedDuringStartup{}
			}
		}

		if clk.Since(start) >= timeout {
			return &ErrReadyTimeout{Check: check, Waited: clk.Since(start), LastErr: lastErr}
		}

		select {
		case <-clk.After(interval):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

type readyProbe struct {
	check  ReadyCheck
	client *http.Client
	dialer *net.Dialer
}

func newReadyProbe(check ReadyCheck, interval, overallTimeout time.Duration) *readyProbe {
	dialTimeout := max(interval*4, time.Second)
	if overallTimeout > 0 && dialTimeout > overallTimeout {
		dialTimeout = overallTimeout
	}

	return &readyProbe{
		check:  check,
		dialer: &net.Dialer{Timeout: dialTimeout},
		client: &http.Client{
			Timeout: dialTimeout,
			Transport: &http.Transport{
				DisableKeepAlives: true,
				DialContext:       (&net.Dialer{Timeout: dialTimeout}).DialContext,
				// #nosec G402 -- opt-in only, via the explicit --insecure flag.
				TLSClientConfig: &tls.Config{InsecureSkipVerify: check.Insecure, MinVersion: tls.VersionTLS12},
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (p *readyProbe) attempt(ctx context.Context) error {
	if p.check.URL != "" {
		return p.attemptHTTP(ctx)
	}
	return p.attemptTCP(ctx)
}

func (p *readyProbe) attemptHTTP(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.check.URL, nil)
	if err != nil {
		return err
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()

	// A 4xx still proves the server is up and serving; only a 5xx suggests it is
	// still coming up.
	if resp.StatusCode >= 500 {
		return fmt.Errorf("readiness endpoint returned %d", resp.StatusCode)
	}
	return nil
}

func (p *readyProbe) attemptTCP(ctx context.Context) error {
	conn, err := p.dialer.DialContext(ctx, "tcp", p.check.Addr)
	if err != nil {
		return err
	}
	return conn.Close()
}

func (p *readyProbe) close() {
	if t, ok := p.client.Transport.(*http.Transport); ok {
		t.CloseIdleConnections()
	}
}

// IsReadyTimeout reports whether err came from a readiness timeout.
func IsReadyTimeout(err error) bool {
	var target *ErrReadyTimeout
	return errors.As(err, &target)
}
