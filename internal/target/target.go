package target

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/shutdowncheck/shutdowncheck/internal/analyze"
)

// Signal is a portable termination signal name.
type Signal string

const (
	SIGTERM Signal = "TERM"
	SIGINT  Signal = "INT"
	SIGQUIT Signal = "QUIT"
	SIGKILL Signal = "KILL"
)

// Valid reports whether s is a signal the tool knows how to deliver.
func (s Signal) Valid() bool {
	switch s {
	case SIGTERM, SIGINT, SIGQUIT, SIGKILL:
		return true
	default:
		return false
	}
}

// Graceful reports whether the signal asks a process to stop rather than
// destroying it.
func (s Signal) Graceful() bool { return s.Valid() && s != SIGKILL }

// Descriptor identifies a target in reports.
type Descriptor struct {
	Kind  analyze.TargetKind
	Label string
	PID   int
}

// ExitStatus describes how a target ended.
//
// Known is false when the target was attached to by PID rather than spawned.
// A process that is not our child cannot be waited on, so its exit code is
// genuinely unavailable — and reporting an unavailable code as 0 would let
// SC013 silently pass on evidence the tool never had.
type ExitStatus struct {
	Known        bool
	Code         int
	Signaled     bool
	TerminatedBy Signal
}

// String renders the status for humans.
func (e ExitStatus) String() string {
	switch {
	case !e.Known:
		return "unknown"
	case e.Signaled:
		return "killed by SIG" + string(e.TerminatedBy)
	default:
		return fmt.Sprintf("exit %d", e.Code)
	}
}

// LogSink receives a captured line of target output.
//
// Capture is push-based rather than an io.Reader the caller polls: the arrival
// instant is what lets a report line up "closing listener" in the target's own
// log against what its sockets actually did, and a pull-based reader would lose
// it.
type LogSink func(stream, line string)

// Target is anything that can be terminated and observed.
//
// Keeping every target kind behind this one interface is what makes Docker and
// Kubernetes additive rather than invasive; nothing outside this package may
// branch on target kind. See docs/adr/0002-target-interface-and-docker-in-v1.md.
type Target interface {
	// Describe identifies the target for the report.
	Describe() Descriptor

	// Start launches the target. It is a no-op for targets that are attached to
	// rather than spawned.
	Start(ctx context.Context) error

	// Signal delivers a termination signal.
	Signal(sig Signal) error

	// Alive reports whether the target is still running.
	Alive() (bool, error)

	// Wait blocks until the target exits or the context ends.
	Wait(ctx context.Context) (ExitStatus, error)

	// DefaultGracePeriod is what a real orchestrator would allow this kind of
	// target before escalating to SIGKILL.
	DefaultGracePeriod() time.Duration

	// Close releases everything the target owns. It must be safe to call more
	// than once, and must leave no surviving child processes.
	Close() error
}

// Errors returned by target construction and operation.
var (
	// ErrUnsupportedPlatform reports that a target kind cannot work on this OS.
	ErrUnsupportedPlatform = errors.New("target kind is not supported on this platform")

	// ErrNotStarted reports an operation on a target that was never started.
	ErrNotStarted = errors.New("target has not been started")

	// ErrAlreadyStarted reports a second call to Start.
	ErrAlreadyStarted = errors.New("target has already been started")

	// ErrExitCodeUnavailable reports that a target's exit code cannot be
	// observed because the process is not a child of this one.
	ErrExitCodeUnavailable = errors.New("exit code is unavailable for an attached process")
)

// UnsafeTargetError reports a target the tool refuses to act on.
type UnsafeTargetError struct {
	Reason string
}

func (e *UnsafeTargetError) Error() string { return "refusing to target: " + e.Reason }

// ValidatePID rejects process identifiers that must never be signalled.
//
// This tool's whole job is to send real signals, including SIGKILL, so the
// guard rails matter: signalling PID 1 would take down the host or container,
// and signalling ourselves would kill the run mid-measurement.
func ValidatePID(pid, selfPID int) error {
	switch {
	case pid <= 0:
		return &UnsafeTargetError{Reason: fmt.Sprintf("PID %d is not a valid process id", pid)}
	case pid == 1:
		return &UnsafeTargetError{Reason: "PID 1 is the init process; signalling it would terminate the host or container"}
	case pid == selfPID:
		return &UnsafeTargetError{Reason: "that PID is shutdowncheck itself"}
	default:
		return nil
	}
}
