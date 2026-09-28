package probe

import (
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/timeline"
)

// Sink receives events as they are observed.
type Sink func(timeline.Event)

// connTracker assigns connection identities and classifies how each connection
// ended.
//
// Request-level success is not enough to judge a drain: a response can complete
// on a socket the server then destroys, which breaks the *next* request the
// client sends on it. Distinguishing a clean FIN from an RST is what makes that
// visible.
type connTracker struct {
	nextID atomic.Uint64
	sink   Sink
	now    func() time.Duration
}

func newConnTracker(sink Sink, now func() time.Duration) *connTracker {
	return &connTracker{sink: sink, now: now}
}

func (t *connTracker) emit(e timeline.Event) {
	if t.sink != nil {
		t.sink(e)
	}
}

// wrap adopts a freshly dialled connection.
func (t *connTracker) wrap(c net.Conn) net.Conn {
	return &trackedConn{Conn: c, id: t.nextID.Add(1), tracker: t}
}

// trackedConn observes the read path so that the manner of the connection's
// death is known by the time it is closed.
type trackedConn struct {
	net.Conn
	id      uint64
	tracker *connTracker

	mu          sync.Mutex
	lastReadErr error
	serverClose bool
	closed      bool
}

func (c *trackedConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if err != nil {
		c.mu.Lock()
		c.lastReadErr = err
		c.mu.Unlock()
	}
	return n, err
}

// markServerClose notes that the server asked for the connection to be closed.
func (c *trackedConn) markServerClose() {
	c.mu.Lock()
	c.serverClose = true
	c.mu.Unlock()
}

func (c *trackedConn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	readErr, serverClose := c.lastReadErr, c.serverClose
	c.mu.Unlock()

	err := c.Conn.Close()

	c.tracker.emit(timeline.ConnectionAt(c.tracker.now(), timeline.ConnectionEvent{
		ID:          c.id,
		Phase:       timeline.ConnClose,
		Termination: classifyTermination(readErr),
		ServerClose: serverClose,
	}))
	return err
}

// classifyTermination decides how a connection ended from the last error seen
// on its read path.
//
// TermUnknown means the client closed first and the server never got the
// chance to signal anything; that is not a defect, so it must not be conflated
// with a clean server-side FIN.
func classifyTermination(readErr error) timeline.ConnTermination {
	switch {
	case readErr == nil:
		return timeline.TermUnknown
	case errors.Is(readErr, io.EOF), errors.Is(readErr, io.ErrUnexpectedEOF):
		return timeline.TermFIN
	case matchesErrno(readErr, syscall.ECONNRESET, wsaeConnReset),
		matchesErrno(readErr, syscall.ECONNABORTED, 0):
		return timeline.TermRST
	case isTimeout(readErr):
		return timeline.TermTimeout
	default:
		return timeline.TermUnknown
	}
}

// trackedFrom digs a tracked connection out of whatever the transport handed
// back. With TLS the connection is wrapped, so a direct type assertion would
// silently lose connection identity on every https target.
func trackedFrom(c net.Conn) (*trackedConn, bool) {
	type netConner interface{ NetConn() net.Conn }

	for range 4 {
		if c == nil {
			return nil, false
		}
		if tc, ok := c.(*trackedConn); ok {
			return tc, true
		}
		nc, ok := c.(netConner)
		if !ok {
			return nil, false
		}
		c = nc.NetConn()
	}
	return nil, false
}
