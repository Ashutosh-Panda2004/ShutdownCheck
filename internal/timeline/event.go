package timeline

import "time"

// Kind discriminates the event union. Every event carries exactly one payload,
// matching its kind.
type Kind string

// KindMeta and the other kind constants identify event payload variants.
const (
	KindMeta       Kind = "meta"
	KindStage      Kind = "stage"
	KindRequest    Kind = "request"
	KindConnection Kind = "connection"
	KindReadiness  Kind = "readiness"
	KindListener   Kind = "listener"
	KindSignal     Kind = "signal"
	KindProcess    Kind = "process"
	KindLog        Kind = "log"
	KindNotice     Kind = "notice"
)

// Stage names a step of the run state machine (spec section 7.4). Recording
// stage transitions lets a report explain what the tool itself was doing at any
// instant, which matters when a user disputes a verdict.
type Stage string

// StagePreflight and the other stage constants identify runner state changes.
const (
	StagePreflight   Stage = "preflight"
	StageStartTarget Stage = "start_target"
	StageWaitReady   Stage = "wait_ready"
	StageWarmup      Stage = "warmup"
	StageCalibrate   Stage = "calibrate"
	StageSteady      Stage = "steady"
	StagePreStop     Stage = "prestop"
	StageSignal      Stage = "signal"
	StageObserve     Stage = "observe"
	StageSigkill     Stage = "sigkill"
	StagePostExit    Stage = "post_exit"
	StageComplete    Stage = "complete"
)

// Outcome classifies how a network operation ended. The distinctions are not
// cosmetic: "refused" means the listener was closed, "reset" means the server
// destroyed an established connection, and "timeout" means it accepted the
// connection and then never answered. Those are three different bugs.
type Outcome string

// OutcomeOK and the other outcome constants classify network results.
const (
	OutcomeOK        Outcome = "ok"
	OutcomeHTTPError Outcome = "http_error"
	OutcomeRefused   Outcome = "refused"
	OutcomeReset     Outcome = "reset"
	OutcomeTimeout   Outcome = "timeout"
	OutcomeEOF       Outcome = "eof"
	OutcomeTLSError  Outcome = "tls_error"
	OutcomeDNSError  Outcome = "dns_error"
	OutcomeAbandoned Outcome = "abandoned"
	OutcomeOther     Outcome = "other"
)

// Succeeded reports whether the outcome represents a served request.
func (o Outcome) Succeeded() bool { return o == OutcomeOK }

// ConnPhase marks a point in a connection's life.
type ConnPhase string

// ConnOpen and the other connection phases identify socket lifecycle events.
const (
	ConnOpen  ConnPhase = "open"
	ConnReuse ConnPhase = "reuse"
	ConnClose ConnPhase = "close"
)

// ConnTermination records how a connection ended. A clean FIN after a
// Connection: close header is orderly shutdown; an RST is the server destroying
// a socket the client still considered usable.
type ConnTermination string

// TermFIN and the other termination constants classify how a socket ended.
const (
	TermFIN     ConnTermination = "fin"
	TermRST     ConnTermination = "rst"
	TermTimeout ConnTermination = "timeout"
	TermUnknown ConnTermination = "unknown"
)

// ProcPhase marks a point in the target process's life.
type ProcPhase string

// ProcStarted and the other process phases identify process lifecycle events.
const (
	ProcStarted      ProcPhase = "started"
	ProcReady        ProcPhase = "ready"
	ProcExited       ProcPhase = "exited"
	ProcPortReleased ProcPhase = "port_released"
)

// Meta describes the run. It is emitted as the first line of the NDJSON stream.
type Meta struct {
	ToolVersion      string           `json:"tool_version"`
	StartedAt        time.Time        `json:"started_at"`
	Target           string           `json:"target,omitempty"`
	Profile          string           `json:"profile,omitempty"`
	Seed             int64            `json:"seed"`
	Trial            int              `json:"trial"`
	Trials           int              `json:"trials"`
	Dropped          int              `json:"dropped,omitempty"`
	DroppedAuxiliary int              `json:"dropped_auxiliary,omitempty"`
	Analysis         *AnalysisContext `json:"analysis,omitempty"`
}

// AnalysisContext is the effective, sanitized interpretation context stored
// with evidence so a default replay reproduces the original report. It remains
// separate from events so an explicit profile can still re-judge them.
type AnalysisContext struct {
	Version int            `json:"version"`
	Target  AnalysisTarget `json:"target"`
	Probe   AnalysisProbe  `json:"probe"`
	Load    AnalysisLoad   `json:"load"`
	Policy  AnalysisPolicy `json:"policy"`
	Trials  AnalysisTrials `json:"trials"`
}

// AnalysisTarget is the report-safe target metadata used by analysis.
type AnalysisTarget struct {
	Kind          string `json:"kind,omitempty"`
	Label         string `json:"label,omitempty"`
	PID           int    `json:"pid,omitempty"`
	DetectedStack string `json:"detected_stack,omitempty"`
}

// AnalysisProbe is the report-safe probe metadata used by analysis.
type AnalysisProbe struct {
	URL          string `json:"url,omitempty"`
	Method       string `json:"method,omitempty"`
	ReadinessURL string `json:"readiness_url,omitempty"`
	Insecure     bool   `json:"insecure"`
}

// AnalysisLoad is the calibrated load metadata used by analysis.
type AnalysisLoad struct {
	Calibrated      bool          `json:"calibrated"`
	RPS             float64       `json:"rps"`
	TargetInFlight  int           `json:"target_in_flight"`
	BaselineLatency time.Duration `json:"baseline_latency_ns"`
	GoalEvaluated   bool          `json:"goal_evaluated"`
	Achievable      bool          `json:"achievable"`
	Warnings        []string      `json:"warnings,omitempty"`
}

// AnalysisPolicy is a serialization-safe copy of the effective policy.
type AnalysisPolicy struct {
	Profile                   string             `json:"profile"`
	AcceptWindow              time.Duration      `json:"accept_window_ns"`
	RequireAcceptDuringWindow bool               `json:"require_accept_during_window"`
	DeregMin                  time.Duration      `json:"dereg_min_ns"`
	ReadinessFlipBudget       time.Duration      `json:"readiness_flip_budget_ns"`
	RequireReadinessFlip      bool               `json:"require_readiness_flip"`
	GracePeriod               time.Duration      `json:"grace_period_ns"`
	MaxShutdownTime           *time.Duration     `json:"max_shutdown_time_ns,omitempty"`
	MinInFlightSample         int                `json:"min_in_flight_sample"`
	MaxInFlightDropPct        float64            `json:"max_inflight_drop_pct"`
	MinScore                  *int               `json:"min_score,omitempty"`
	LatencySpikeFactor        float64            `json:"latency_spike_factor"`
	Severities                []AnalysisSeverity `json:"severities,omitempty"`
}

// AnalysisSeverity is one explicit signature severity override.
type AnalysisSeverity struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
}

// AnalysisTrials preserves the aggregate summary for the selected worst trial.
type AnalysisTrials struct {
	Total      int  `json:"total"`
	Failed     int  `json:"failed"`
	Consistent bool `json:"consistent"`
}

// Event is one observation. Offset is a monotonic duration from run start and
// is stamped by the producer at the moment of observation, never by the
// recorder: a recorder timestamp would absorb lock and channel delay into the
// measurement, and the whole tool turns on sub-millisecond correlation.
type Event struct {
	Seq    uint64        `json:"seq"`
	Offset time.Duration `json:"offset_ns"`
	Kind   Kind          `json:"kind"`

	Meta       *Meta            `json:"meta,omitempty"`
	Stage      *StageEvent      `json:"stage,omitempty"`
	Request    *RequestEvent    `json:"request,omitempty"`
	Connection *ConnectionEvent `json:"connection,omitempty"`
	Readiness  *ReadinessEvent  `json:"readiness,omitempty"`
	Listener   *ListenerEvent   `json:"listener,omitempty"`
	Signal     *SignalEvent     `json:"signal,omitempty"`
	Process    *ProcessEvent    `json:"process,omitempty"`
	Log        *LogEvent        `json:"log,omitempty"`
	Notice     *NoticeEvent     `json:"notice,omitempty"`
}

// StageEvent records entry into a run stage.
type StageEvent struct {
	Stage Stage `json:"stage"`
}

// RequestEvent is a completed request attempt. Both timestamps are kept on one
// record because phase classification needs the pair: a request is "in flight
// at the signal" exactly when Sent < signal < Done.
type RequestEvent struct {
	ID         uint64        `json:"id"`
	Definition string        `json:"definition,omitempty"`
	Method     string        `json:"method"`
	URL        string        `json:"url"`
	Scheduled  time.Duration `json:"scheduled_ns"`
	Sent       time.Duration `json:"sent_ns"`
	Done       time.Duration `json:"done_ns"`
	Status     int           `json:"status,omitempty"`
	Outcome    Outcome       `json:"outcome"`
	Error      string        `json:"error,omitempty"`
	ConnID     uint64        `json:"conn_id,omitempty"`
	ConnReused bool          `json:"conn_reused,omitempty"`
	Warmup     bool          `json:"warmup,omitempty"`
}

// Latency is how long the attempt took once it was actually dispatched.
func (r RequestEvent) Latency() time.Duration { return r.Done - r.Sent }

// QueueDelay is how long the request waited inside the load generator before
// being dispatched.
//
// It measures the tool, not the target. A rising queue delay means the
// generator could not maintain its offered rate, which is the signature of
// coordinated omission and makes the run less trustworthy rather than making
// the service look worse.
func (r RequestEvent) QueueDelay() time.Duration {
	if r.Scheduled == 0 || r.Sent < r.Scheduled {
		return 0
	}
	return r.Sent - r.Scheduled
}

// ConnectionEvent records a point in a connection's life, including the
// forensic details that distinguish orderly drain from socket destruction.
type ConnectionEvent struct {
	ID           uint64          `json:"id"`
	Phase        ConnPhase       `json:"phase"`
	RemoteAddr   string          `json:"remote_addr,omitempty"`
	Reused       bool            `json:"reused,omitempty"`
	ServerClose  bool            `json:"server_connection_close,omitempty"`
	Termination  ConnTermination `json:"termination,omitempty"`
	TLSHandshake time.Duration   `json:"tls_handshake_ns,omitempty"`
}

// ReadinessEvent is one poll of the readiness endpoint.
type ReadinessEvent struct {
	Status  int           `json:"status,omitempty"`
	Healthy bool          `json:"healthy"`
	Outcome Outcome       `json:"outcome"`
	Latency time.Duration `json:"latency_ns"`
}

// ListenerEvent is one raw TCP dial, used to determine when the socket stopped
// accepting independently of whether the application still answers HTTP.
type ListenerEvent struct {
	Accepting bool          `json:"accepting"`
	Outcome   Outcome       `json:"outcome"`
	Latency   time.Duration `json:"latency_ns"`
}

// SignalEvent records signal delivery. Skew is the gap between the intended and
// actual delivery instant; a large skew means the host was too loaded for the
// run to be trusted.
type SignalEvent struct {
	Signal string        `json:"signal"`
	Skew   time.Duration `json:"skew_ns"`
	Error  string        `json:"error,omitempty"`
}

// ProcessEvent records a transition in the target process's life.
type ProcessEvent struct {
	Phase        ProcPhase `json:"phase"`
	PID          int       `json:"pid,omitempty"`
	ExitCode     *int      `json:"exit_code,omitempty"`
	TerminatedBy string    `json:"terminated_by,omitempty"`
}

// LogEvent is one captured line of target output, interleaved into the timeline
// so a report can line up "closing listener" against what the sockets did.
type LogEvent struct {
	Stream string `json:"stream"`
	Line   string `json:"line"`
}

// NoticeEvent is a tool-level remark: a calibration decision, a degraded
// capability, or a reason the run may not be trustworthy.
type NoticeEvent struct {
	Level   string `json:"level"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

// Constructors keep producers from building malformed events by hand: an event
// whose Kind disagrees with its payload would silently vanish from analysis.

// StageAt returns a stage-transition event.
func StageAt(offset time.Duration, s Stage) Event {
	return Event{Offset: offset, Kind: KindStage, Stage: &StageEvent{Stage: s}}
}

// RequestAt returns a completed-request event, stamped at completion.
func RequestAt(r RequestEvent) Event {
	return Event{Offset: r.Done, Kind: KindRequest, Request: &r}
}

// ConnectionAt returns a connection-lifecycle event.
func ConnectionAt(offset time.Duration, c ConnectionEvent) Event {
	return Event{Offset: offset, Kind: KindConnection, Connection: &c}
}

// ReadinessAt returns a readiness-poll event.
func ReadinessAt(offset time.Duration, r ReadinessEvent) Event {
	return Event{Offset: offset, Kind: KindReadiness, Readiness: &r}
}

// ListenerAt returns a listener-probe event.
func ListenerAt(offset time.Duration, l ListenerEvent) Event {
	return Event{Offset: offset, Kind: KindListener, Listener: &l}
}

// SignalAt returns a signal-delivery event.
func SignalAt(offset time.Duration, s SignalEvent) Event {
	return Event{Offset: offset, Kind: KindSignal, Signal: &s}
}

// ProcessAt returns a process-lifecycle event.
func ProcessAt(offset time.Duration, p ProcessEvent) Event {
	return Event{Offset: offset, Kind: KindProcess, Process: &p}
}

// LogAt returns a captured target log line.
func LogAt(offset time.Duration, stream, line string) Event {
	return Event{Offset: offset, Kind: KindLog, Log: &LogEvent{Stream: stream, Line: line}}
}

// NoticeAt returns a tool-level notice.
func NoticeAt(offset time.Duration, level, code, message string) Event {
	return Event{
		Offset: offset,
		Kind:   KindNotice,
		Notice: &NoticeEvent{Level: level, Code: code, Message: message},
	}
}
