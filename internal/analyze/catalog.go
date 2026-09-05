package analyze

import (
	"sort"

	"github.com/shutdowncheck/shutdowncheck/pkg/schema"
)

// Stage identifies which of the seven stages of correct termination a signature
// belongs to. Naming the broken stage is what turns "requests failed" into
// something a developer can act on.
type Stage string

// StageSignalReceived and the other stage constants identify the shutdown
// stage associated with a signature.
const (
	StageSignalReceived Stage = "S1" // handle SIGTERM at all
	StageReadinessFlip  Stage = "S2" // start failing readiness immediately
	StageLameDuck       Stage = "S3" // keep serving while de-registration propagates
	StageListenerClosed Stage = "S4" // stop accepting once the window elapses
	StageConnClose      Stage = "S5" // signal close: Connection: close / GOAWAY
	StageDrain          Stage = "S6" // finish in-flight work
	StageCleanExit      Stage = "S7" // exit inside the grace budget, no orphans
	StageNone           Stage = ""   // not tied to a single stage
)

// SignatureID names a failure mode. IDs are permanent: they appear in reports,
// in CI configuration, and in documentation URLs, so one is never reused for a
// different meaning.
type SignatureID string

// The failure signature catalogue, per spec section 8.
const (
	SC000 SignatureID = "SC000" // INSUFFICIENT_INFLIGHT
	SC001 SignatureID = "SC001" // SIGTERM_IGNORED
	SC002 SignatureID = "SC002" // SIGKILL_REQUIRED
	SC003 SignatureID = "SC003" // IN_FLIGHT_DROPPED
	SC004 SignatureID = "SC004" // ABRUPT_CONNECTION_RESET
	SC005 SignatureID = "SC005" // LISTENER_OPEN_AFTER_WINDOW
	SC006 SignatureID = "SC006" // NO_DEREGISTRATION_WINDOW
	SC007 SignatureID = "SC007" // READINESS_NOT_FLIPPED
	SC008 SignatureID = "SC008" // READINESS_FLIP_SLOW
	SC009 SignatureID = "SC009" // KEEPALIVE_NOT_TERMINATED
	SC010 SignatureID = "SC010" // SHUTDOWN_BUDGET_EXCEEDED
	SC011 SignatureID = "SC011" // EARLY_EXIT
	SC012 SignatureID = "SC012" // PORT_HELD_AFTER_EXIT
	SC013 SignatureID = "SC013" // NONZERO_EXIT_CODE
	SC014 SignatureID = "SC014" // DRAIN_LATENCY_SPIKE
	SC015 SignatureID = "SC015" // ACCEPT_WITHOUT_RESPONSE
	SC016 SignatureID = "SC016" // READINESS_FLAPPED
	SC017 SignatureID = "SC017" // POST_KILL_TRAFFIC_LOSS
)

// SignatureInfo is the static description of a failure mode. The detection
// rules arrive in Phase 4; this metadata exists now so configuration can
// validate --fail-on and --ignore against real identifiers rather than
// accepting typos silently.
type SignatureInfo struct {
	ID      SignatureID
	Name    string
	Stage   Stage
	Summary string
	Impact  string
}

var catalog = map[SignatureID]SignatureInfo{
	SC000: {SC000, "INSUFFICIENT_INFLIGHT", StageNone,
		"The run did not retain enough trustworthy baseline and in-flight evidence.",
		"No shutdown conclusion is safe from incomplete or already-unhealthy evidence. Never reported as a pass."},
	SC001: {SC001, "SIGTERM_IGNORED", StageSignalReceived,
		"The process showed no reaction to the signal at all.",
		"Every deploy hard-kills the process and destroys all in-flight work."},
	SC002: {SC002, "SIGKILL_REQUIRED", StageCleanExit,
		"The process was still alive when the grace period expired.",
		"The orchestrator SIGKILLs the service on every deploy, and deploys are slower."},
	SC003: {SC003, "IN_FLIGHT_DROPPED", StageDrain,
		"Requests that were already being processed failed during shutdown.",
		"In-progress user requests fail on every deploy."},
	SC004: {SC004, "ABRUPT_CONNECTION_RESET", StageConnClose,
		"Connections were destroyed with RST instead of being closed cleanly.",
		"Clients see ECONNRESET, which breaks retries for non-idempotent requests."},
	SC005: {SC005, "LISTENER_OPEN_AFTER_WINDOW", StageListenerClosed,
		"The listener kept accepting connections past the de-registration window.",
		"The service keeps pulling in traffic it cannot finish, so drain never completes."},
	SC006: {SC006, "NO_DEREGISTRATION_WINDOW", StageLameDuck,
		"The listener closed almost immediately after the signal.",
		"502s at the ingress on every deploy, because routing updates land afterwards."},
	SC007: {SC007, "READINESS_NOT_FLIPPED", StageReadinessFlip,
		"The readiness endpoint stayed healthy for the whole shutdown.",
		"The load balancer keeps routing to a dying instance for a full probe interval."},
	SC008: {SC008, "READINESS_FLIP_SLOW", StageReadinessFlip,
		"Readiness took too long to start failing.",
		"Extra seconds of traffic are sent to an instance that is going away."},
	SC009: {SC009, "KEEPALIVE_NOT_TERMINATED", StageConnClose,
		"Responses after the signal did not ask clients to close the connection.",
		"Clients keep reusing a socket that is about to die, so the next request fails."},
	SC010: {SC010, "SHUTDOWN_BUDGET_EXCEEDED", StageCleanExit,
		"Shutdown took longer than the declared budget.",
		"Slow deploys and rollbacks, and a longer window of partial availability."},
	SC011: {SC011, "EARLY_EXIT", StageDrain,
		"The process exited while requests were still in flight.",
		"The service abandons work it accepted, causing silent loss on write paths."},
	SC012: {SC012, "PORT_HELD_AFTER_EXIT", StageCleanExit,
		"The port was still accepting connections after the main process exited.",
		"An orphaned child holds the port, so restarts fail or traffic reaches a zombie."},
	SC013: {SC013, "NONZERO_EXIT_CODE", StageCleanExit,
		"The process exited with a non-zero status after being asked to stop.",
		"The orchestrator records a crash, which can affect restart accounting."},
	SC014: {SC014, "DRAIN_LATENCY_SPIKE", StageDrain,
		"Latency rose sharply while draining.",
		"Upstream callers time out even where requests technically complete."},
	SC015: {SC015, "ACCEPT_WITHOUT_RESPONSE", StageListenerClosed,
		"Connections were accepted after the signal but never answered.",
		"Callers hang for their full timeout instead of failing fast."},
	SC016: {SC016, "READINESS_FLAPPED", StageReadinessFlip,
		"Readiness recovered after starting to fail.",
		"The load balancer re-registers a terminating instance and sends it traffic again."},
	SC017: {SC017, "POST_KILL_TRAFFIC_LOSS", StageCleanExit,
		"Requests were still in flight when SIGKILL landed.",
		"Quantifies exactly what the hard kill destroyed."},
}

// Catalog returns every signature, ordered by ID.
//
// Ordering matters: reports must be byte-identical across runs, and Go
// randomises map iteration.
func Catalog() []SignatureInfo {
	out := make([]SignatureInfo, 0, len(catalog))
	for _, info := range catalog {
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Lookup returns the catalogue entry for an identifier.
func Lookup(id SignatureID) (SignatureInfo, bool) {
	info, ok := catalog[id]
	return info, ok
}

// DefaultSeverity is the severity a signature carries before profile-specific
// adjustments and user overrides.
func DefaultSeverity(id SignatureID) schema.Severity {
	switch id {
	case SC001, SC002, SC003, SC004, SC005, SC011, SC012, SC015:
		return schema.SeverityError
	case SC000, SC017:
		return schema.SeverityInfo
	default:
		return schema.SeverityWarn
	}
}
