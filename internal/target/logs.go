package target

import (
	"fmt"
	"io"
	"sync"

	"github.com/shutdowncheck/shutdowncheck/internal/redact"
)

// Log capture limits. The tool has no control over what a service prints, so
// both the per-line and the total sizes are bounded.
const (
	DefaultMaxLogBytes = 1 << 20
	maxLogLineBytes    = 8 << 10
	maxLogLines        = 10_000
)

// lineWriter turns a byte stream into redacted lines pushed to a sink.
//
// Capture is wired through cmd.Stdout rather than cmd.StdoutPipe because Wait
// closes a pipe as soon as the process exits, which can truncate exactly the
// shutdown log lines the report most needs. Letting exec.Cmd own the copy means
// Wait returns only once every byte has been handed over.
type lineWriter struct {
	stream string
	sink   LogSink
	max    int64

	mu     sync.Mutex
	buf    []byte
	total  int64
	lines  int
	capped bool
}

// NewLogWriter returns a writer that emits complete redacted lines to sink.
func NewLogWriter(stream string, maxBytes int64, sink LogSink) io.Writer {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxLogBytes
	}
	return &lineWriter{stream: stream, sink: sink, max: maxBytes}
}

func (w *lineWriter) Write(p []byte) (int, error) {
	if w.sink == nil {
		return len(p), nil
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	for _, b := range p {
		if b == '\n' {
			w.emitLocked()
			continue
		}
		if b == '\r' {
			continue
		}
		if len(w.buf) < maxLogLineBytes {
			w.buf = append(w.buf, b)
			continue
		}
		// Past the per-line cap: drop the rest of the line rather than let one
		// runaway line exhaust memory.
		if len(w.buf) == maxLogLineBytes {
			w.buf = append(w.buf, []byte(" ...[line truncated]")...)
		}
	}
	return len(p), nil
}

// Flush emits any trailing partial line, for output that ended without a
// newline.
func (w *lineWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.emitLocked()
}

func flushLogWriter(w io.Writer) {
	if flusher, ok := w.(interface{ Flush() }); ok {
		flusher.Flush()
	}
}

func (w *lineWriter) emitLocked() {
	if len(w.buf) == 0 {
		return
	}
	line := string(w.buf)
	w.buf = w.buf[:0]

	size := int64(len(line) + 1)
	if w.total+size > w.max || w.lines >= maxLogLines {
		if !w.capped {
			w.capped = true
			w.sink(w.stream, fmt.Sprintf(
				"[shutdowncheck] log capture truncated after %d bytes or %d lines", w.max, maxLogLines))
		}
		return
	}

	w.total += size
	w.lines++
	w.sink(w.stream, redact.Message(line))
}
