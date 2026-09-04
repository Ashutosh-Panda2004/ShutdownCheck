package schema

import "time"

// SchemaVersion identifies the report contract. Additive changes bump the minor
// version; removing or repurposing a field requires a major bump and an ADR.
// See docs/adr/0009-public-versioned-report-schema.md.
//
// 1.1 added probe.insecure.
const SchemaVersion = "1.1"

// WeightsVersion identifies the scoring weight table used to produce Score.
// Publishing it means any score can be reproduced and audited later.
const WeightsVersion = "1.0"

// Verdict is the top-level result.
type Verdict string

const (
	// VerdictPass means correct termination behaviour was positively
	// demonstrated.
	VerdictPass Verdict = "pass"
	// VerdictFail means at least one error-severity signature fired or a gate
	// threshold was breached.
	VerdictFail Verdict = "fail"
	// VerdictInconclusive means the experiment was not valid. It is emphatically
	// not a pass: reporting one would manufacture confidence in a service that
	// may drop traffic on every deploy.
	VerdictInconclusive Verdict = "inconclusive"
)

// Exit codes, per spec section 9.3. A pipeline needs to distinguish "this build
// has a shutdown bug" from "the experiment did not run", so these are separate
// values rather than a catch-all 1.
const (
	ExitPass         = 0
	ExitFail         = 1
	ExitInconclusive = 2
	ExitUsage        = 3
	ExitTarget       = 4
	ExitInternal     = 5
	ExitInterrupted  = 130
)

// ExitCode maps a verdict to its process exit code.
func (v Verdict) ExitCode() int {
	switch v {
	case VerdictPass:
		return ExitPass
	case VerdictFail:
		return ExitFail
	case VerdictInconclusive:
		return ExitInconclusive
	default:
		return ExitInternal
	}
}

// Severity controls whether a finding fails the run.
type Severity string

const (
	// SeverityError fails the run.
	SeverityError Severity = "error"
	// SeverityWarn is reported and deducts score, but does not fail the run.
	SeverityWarn Severity = "warn"
	// SeverityInfo is contextual only.
	SeverityInfo Severity = "info"
)

// Report is the complete result of a run.
type Report struct {
	SchemaVersion string `json:"schema_version"`
	ToolVersion   string `json:"tool_version"`

	Verdict Verdict `json:"verdict"`
	Score   Score   `json:"score"`

	Run         Run         `json:"run"`
	Target      Target      `json:"target"`
	Probe       Probe       `json:"probe"`
	Load        Load        `json:"load"`
	Timeline    Timeline    `json:"timeline"`
	Requests    Requests    `json:"requests"`
	Connections Connections `json:"connections"`
	Process     Process     `json:"process"`

	Findings []Finding `json:"findings"`
	Gates    []Gate    `json:"gates"`
}

// Score is the 0-100 summary. It is informational: the verdict comes from
// rules, not from the score, unless the user sets an explicit --min-score gate.
type Score struct {
	Value          int    `json:"value"`
	Grade          string `json:"grade"`
	WeightsVersion string `json:"weights_version"`
}

// Grade converts a score to its letter band.
func Grade(value int) string {
	switch {
	case value >= 90:
		return "A"
	case value >= 80:
		return "B"
	case value >= 70:
		return "C"
	case value >= 60:
		return "D"
	default:
		return "F"
	}
}

// Run describes the experiment itself.
type Run struct {
	StartedAt time.Time `json:"started_at"`
	Profile   string    `json:"profile"`
	Seed      int64     `json:"seed"`
	Trials    Trials    `json:"trials"`
}

// Trials reports multi-trial aggregation. An inconsistent result is itself a
// finding: a shutdown that fails one run in three is flaky in production too.
type Trials struct {
	Total      int  `json:"total"`
	Failed     int  `json:"failed"`
	Consistent bool `json:"consistent"`
}

// Target describes what was terminated.
type Target struct {
	Kind          string `json:"kind"`
	Label         string `json:"label"`
	PID           int    `json:"pid,omitempty"`
	DetectedStack string `json:"detected_stack,omitempty"`
	GracePeriodMS int64  `json:"grace_period_ms"`
}

// Probe describes what was requested.
type Probe struct {
	URL          string `json:"url"`
	Method       string `json:"method"`
	ReadinessURL string `json:"readiness_url,omitempty"`
	// Insecure records that certificate verification was disabled. Never
	// omitted: a reader has to be able to tell "verification was on" from
	// "this field predates the change", and silence would read as the former.
	Insecure bool `json:"insecure"`
}

// Load describes the traffic that was generated, including whether the rate was
// calibrated and whether the in-flight goal was actually met.
type Load struct {
	Calibrated               bool     `json:"calibrated"`
	RPS                      float64  `json:"rps"`
	TargetInFlight           int      `json:"target_in_flight"`
	ObservedInFlightAtSignal int      `json:"observed_in_flight_at_signal"`
	BaselineLatencyMS        Latency  `json:"baseline_latency_ms"`
	Warnings                 []string `json:"warnings,omitempty"`
}

// Latency holds percentile summaries in milliseconds.
type Latency struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
}

// Timeline holds the key instants, all in milliseconds relative to signal
// delivery.
//
// Absent measurements are null rather than zero. The difference between
// "readiness flipped immediately" and "readiness never flipped" is the
// difference between a passing and a failing service, and a zero would conflate
// them.
type Timeline struct {
	SignalSentAtMS       int64  `json:"signal_sent_at_ms"`
	ReadinessFlippedAtMS *int64 `json:"readiness_flipped_at_ms"`
	ListenerClosedAtMS   *int64 `json:"listener_closed_at_ms"`
	SigkillSentAtMS      *int64 `json:"sigkill_sent_at_ms"`
	ProcessExitedAtMS    *int64 `json:"process_exited_at_ms"`
	PortReleasedAtMS     *int64 `json:"port_released_at_ms"`
	ShutdownDurationMS   *int64 `json:"shutdown_duration_ms"`
}

// Requests summarises traffic by shutdown phase.
type Requests struct {
	Total          int     `json:"total"`
	ByPhase        Phases  `json:"by_phase"`
	DrainLatencyMS Latency `json:"drain_latency_ms"`
	Dropped        int     `json:"dropped_records,omitempty"`
}

// Phases holds one summary per shutdown phase.
type Phases struct {
	Warmup     PhaseStats `json:"warmup"`
	Steady     PhaseStats `json:"steady"`
	InFlight   PhaseStats `json:"in_flight"`
	PostSignal PhaseStats `json:"post_signal"`
	PostWindow PhaseStats `json:"post_window"`
	AtSigkill  PhaseStats `json:"at_sigkill"`
}

// PhaseStats counts outcomes within one phase. Failures is keyed by outcome so
// that "318 refused" and "318 reset" are never confused: the first means the
// listener was closed, the second means established connections were destroyed.
type PhaseStats struct {
	Count    int            `json:"count"`
	OK       int            `json:"ok"`
	Failed   int            `json:"failed"`
	Failures map[string]int `json:"failures,omitempty"`
}

// Connections summarises connection-layer behaviour during shutdown.
type Connections struct {
	Opened                    int `json:"opened"`
	ReusedAfterSignal         int `json:"reused_after_signal"`
	ClosedWithConnectionClose int `json:"closed_with_connection_close"`
	ClosedByFIN               int `json:"closed_by_fin"`
	ClosedByRST               int `json:"closed_by_rst"`
}

// Process describes how the target ended.
type Process struct {
	ExitCode           *int   `json:"exit_code"`
	TerminatedBySignal string `json:"terminated_by_signal,omitempty"`
	PortHeldAfterExit  bool   `json:"port_held_after_exit"`
}

// Finding is one triggered failure signature.
type Finding struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Stage         string         `json:"stage"`
	Severity      Severity       `json:"severity"`
	Summary       string         `json:"summary"`
	Evidence      map[string]any `json:"evidence,omitempty"`
	Impact        string         `json:"impact,omitempty"`
	RemediationID string         `json:"remediation_id,omitempty"`
	DocsURL       string         `json:"docs_url,omitempty"`
}

// Gate is one user-configured threshold and whether it held.
type Gate struct {
	Name      string  `json:"name"`
	Threshold float64 `json:"threshold"`
	Actual    float64 `json:"actual"`
	Passed    bool    `json:"passed"`
}
