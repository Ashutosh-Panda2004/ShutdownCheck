package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Built-in defaults, per spec section 11. They are deliberately chosen so that
// a first run works without tuning; in particular there is no default request
// rate, because the correct rate depends on the service's own latency and
// guessing it is how a tool produces a meaningless pass.
const (
	DefaultEnsureInFlight = 20
	DefaultMaxRPS         = 2000.0
	DefaultConcurrencyCap = 512
	DefaultWarmup         = 3 * time.Second
	DefaultSteady         = 5 * time.Second
	DefaultRequestTimeout = 10 * time.Second
	DefaultSignal         = "TERM"
	DefaultTrials         = 1
	DefaultListenerProbe  = 20 * time.Millisecond
	DefaultMethod         = "GET"
)

// SchemaVersion is the only supported config file version.
const SchemaVersion = 1

// DefaultConfigPath is looked for when no --config flag is given.
const DefaultConfigPath = "shutdowncheck.yaml"

// Duration is a time.Duration that reads as a YAML string such as "30s".
type Duration time.Duration

// Duration returns the underlying duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// String renders the duration.
func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalYAML parses a duration string, reporting the offending line.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var raw string
	if err := node.Decode(&raw); err != nil {
		return fmt.Errorf("line %d: expected a duration string such as \"30s\"", node.Line)
	}

	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("line %d: %q is not a valid duration (examples: 500ms, 5s, 2m)", node.Line, raw)
	}
	if parsed < 0 {
		return fmt.Errorf("line %d: duration %q cannot be negative", node.Line, raw)
	}

	*d = Duration(parsed)
	return nil
}

// MarshalYAML renders the duration back to its string form.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// File is the parsed shutdowncheck.yaml.
type File struct {
	Version   int                  `yaml:"version"`
	Defaults  Defaults             `yaml:"defaults"`
	Scenarios map[string]*Scenario `yaml:"scenarios"`
}

// Defaults apply to every scenario unless the scenario overrides them.
type Defaults struct {
	Profile           string    `yaml:"profile"`
	GracePeriod       *Duration `yaml:"grace_period"`
	EnsureInFlight    *int      `yaml:"ensure_in_flight"`
	Trials            *int      `yaml:"trials"`
	CaptureTargetLogs *bool     `yaml:"capture_target_logs"`
}

// Scenario is one named experiment.
type Scenario struct {
	Target      TargetSpec      `yaml:"target"`
	Probe       ProbeSpec       `yaml:"probe"`
	Load        LoadSpec        `yaml:"load"`
	Termination TerminationSpec `yaml:"termination"`
	Gate        GateSpec        `yaml:"gate"`
}

// TargetSpec selects what to terminate. Exactly one of command, pid or docker
// must be set.
type TargetSpec struct {
	Command []string   `yaml:"command"`
	PID     int        `yaml:"pid"`
	Docker  string     `yaml:"docker"`
	Ready   *ReadySpec `yaml:"ready"`
}

// ReadySpec describes how to tell that a spawned target has finished starting.
type ReadySpec struct {
	URL     string    `yaml:"url"`
	Port    int       `yaml:"port"`
	Timeout *Duration `yaml:"timeout"`
}

// ProbeSpec describes what to request.
type ProbeSpec struct {
	ReadinessURL string        `yaml:"readiness_url"`
	SlowURL      string        `yaml:"slow_url"`
	Insecure     bool          `yaml:"insecure"`
	Requests     []RequestSpec `yaml:"requests"`
}

// RequestSpec is one request definition in a possibly-weighted mix.
type RequestSpec struct {
	Name     string            `yaml:"name"`
	Weight   int               `yaml:"weight"`
	URL      string            `yaml:"url"`
	Method   string            `yaml:"method"`
	Headers  map[string]string `yaml:"headers"`
	Body     string            `yaml:"body"`
	BodyFile string            `yaml:"body_file"`
}

// LoadSpec controls traffic generation.
type LoadSpec struct {
	EnsureInFlight *int      `yaml:"ensure_in_flight"`
	RPS            *float64  `yaml:"rps"`
	MaxRPS         *float64  `yaml:"max_rps"`
	ConcurrencyCap *int      `yaml:"concurrency_cap"`
	Warmup         *Duration `yaml:"warmup"`
	Steady         *Duration `yaml:"steady"`
	RequestTimeout *Duration `yaml:"request_timeout"`
	KeepAlive      *bool     `yaml:"keep_alive"`
	Seed           *int64    `yaml:"seed"`
}

// TerminationSpec controls how the target is stopped.
type TerminationSpec struct {
	Signal         string    `yaml:"signal"`
	PreStopSleep   *Duration `yaml:"prestop_sleep"`
	GracePeriod    *Duration `yaml:"grace_period"`
	EnforceSigkill *bool     `yaml:"enforce_sigkill"`
	Profile        string    `yaml:"profile"`
	AcceptWindow   *Duration `yaml:"accept_window"`
}

// GateSpec holds the thresholds that decide CI pass or fail.
type GateSpec struct {
	MaxInFlightDropPct *float64  `yaml:"max_inflight_drop_pct"`
	MaxShutdownTime    *Duration `yaml:"max_shutdown_time"`
	MinScore           *int      `yaml:"min_score"`
	FailOn             []string  `yaml:"fail_on"`
	Ignore             []string  `yaml:"ignore"`
	Trials             *int      `yaml:"trials"`
}
