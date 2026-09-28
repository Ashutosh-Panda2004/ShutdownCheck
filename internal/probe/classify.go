package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"syscall"

	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/redact"
	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/timeline"
)

// Windows socket error numbers. Go's portable errno constants do not reliably
// compare equal to WSA errors, so the numeric codes are checked as well; on
// other platforms these values are not valid errnos, so the extra check is
// harmless.
const (
	wsaeConnReset   = 10054
	wsaeTimedOut    = 10060
	wsaeConnRefused = 10061
)

// Classify turns a transport error into an outcome and a safe detail string.
//
// The distinctions carry the diagnosis. "Refused" means the listener was
// already closed, "reset" means an established connection was destroyed
// mid-flight, and "timeout" means the connection was accepted and then never
// answered. Those are three different defects at three different shutdown
// stages, and collapsing them into "failed" would throw away the finding.
func Classify(err error) (timeline.Outcome, string) {
	if err == nil {
		return timeline.OutcomeOK, ""
	}

	detail := redact.Message(err.Error())

	switch {
	case errors.Is(err, context.Canceled):
		return timeline.OutcomeAbandoned, detail

	case isDNSError(err):
		return timeline.OutcomeDNSError, detail

	case isTLSError(err):
		return timeline.OutcomeTLSError, detail

	case matchesErrno(err, syscall.ECONNREFUSED, wsaeConnRefused):
		return timeline.OutcomeRefused, detail

	case matchesErrno(err, syscall.ECONNRESET, wsaeConnReset),
		matchesErrno(err, syscall.ECONNABORTED, 0),
		matchesErrno(err, syscall.EPIPE, 0):
		return timeline.OutcomeReset, detail

	case isTimeout(err):
		return timeline.OutcomeTimeout, detail

	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return timeline.OutcomeEOF, detail

	default:
		return timeline.OutcomeOther, detail
	}
}

// ClassifyStatus maps a response status to an outcome. Only 2xx counts as
// served; a 503 during shutdown is a dropped request from the caller's point of
// view, however deliberate it was.
func ClassifyStatus(status int) timeline.Outcome {
	if status >= 200 && status < 300 {
		return timeline.OutcomeOK
	}
	return timeline.OutcomeHTTPError
}

func isDNSError(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr)
}

func isTLSError(err error) bool {
	var (
		recordErr *tls.RecordHeaderError
		certErr   *tls.CertificateVerificationError
		unknownCA x509.UnknownAuthorityError
		hostErr   x509.HostnameError
		invalid   x509.CertificateInvalidError
	)
	return errors.As(err, &recordErr) ||
		errors.As(err, &certErr) ||
		errors.As(err, &unknownCA) ||
		errors.As(err, &hostErr) ||
		errors.As(err, &invalid)
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if matchesErrno(err, syscall.ETIMEDOUT, wsaeTimedOut) {
		return true
	}

	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// matchesErrno compares against the portable errno and, where given, the
// Windows socket number. A zero windows code means "no separate mapping".
func matchesErrno(err error, portable syscall.Errno, windows uintptr) bool {
	if errors.Is(err, portable) {
		return true
	}
	if windows == 0 {
		return false
	}

	var errno syscall.Errno
	return errors.As(err, &errno) && uintptr(errno) == windows
}
