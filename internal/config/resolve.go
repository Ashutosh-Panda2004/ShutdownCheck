package config

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/shutdowncheck/shutdowncheck/internal/analyze"
	"github.com/shutdowncheck/shutdowncheck/pkg/schema"
)

// Resolved is a scenario with defaults applied and the analysis policy built.
// Everything downstream consumes this, never the raw file, so precedence is
// settled in exactly one place.
type Resolved struct {
	Name              string
	Target            Target
	Probe             Probe
	Traffic           Traffic
	Termination       Termination
	Policy            analyze.Policy
	Trials            int
	CaptureTargetLogs bool
}

// Target is the resolved target selection.
type Target struct {
	Kind    analyze.TargetKind
	Command []string
	PID     int
	Docker  string
	Ready   Ready
	Label   string
}

// Ready describes how to detect that a spawned target has started.
type Ready struct {
	URL     string
	Port    int
	Timeout time.Duration
}

// Probe is the resolved request set.
type Probe struct {
	Requests     []Request
	ReadinessURL string
	SlowURL      string
	Insecure     bool
}

// Request is one resolved request definition.
type Request struct {
	Name     string
	Weight   int
	URL      string
	Method   string
	Headers  map[string]string
	Body     string
	BodyFile string
}

// Traffic is the resolved traffic configuration.
type Traffic struct {
	Calibrate      bool
	EnsureInFlight int
	RPS            float64
	MaxRPS         float64
	ConcurrencyCap int
	Warmup         time.Duration
	Steady         time.Duration
	RequestTimeout time.Duration
	KeepAlive      bool
	Seed           int64
}

// Termination is the resolved shutdown configuration.
type Termination struct {
	Signal         string
	PreStopSleep   time.Duration
	GracePeriod    time.Duration
	EnforceSigkill bool
}

// ScenarioNames lists the scenarios in a file, in a stable order.
func (f *File) ScenarioNames() []string { return sortedKeys(f.Scenarios) }

// Resolve produces the runnable form of a scenario.
//
// An empty name is allowed when the file defines exactly one scenario; that is
// the common case and forcing a redundant --scenario flag would be friction for
// no benefit.
func (f *File) Resolve(name string) (*Resolved, error) {
	if len(f.Scenarios) == 0 {
		return nil, ErrNoScenarios
	}

	if name == "" {
		names := f.ScenarioNames()
		if len(names) > 1 {
			return nil, fmt.Errorf(
				"config defines %d scenarios (%s); choose one with --scenario",
				len(names), strings.Join(names, ", "))
		}
		name = names[0]
	}

	scenario, ok := f.Scenarios[name]
	if !ok {
		return nil, fmt.Errorf("no scenario named %q; this file defines %s",
			name, strings.Join(f.ScenarioNames(), ", "))
	}
	if scenario == nil {
		return nil, fmt.Errorf("scenario %q is empty; it needs at least `target` and `probe`", name)
	}

	resolved := &Resolved{
		Name:              name,
		Target:            resolveTarget(scenario.Target),
		Probe:             resolveProbe(scenario.Probe),
		Traffic:           resolveTraffic(scenario.Load, f.Defaults),
		Trials:            intOr(DefaultTrials, scenario.Gate.Trials, f.Defaults.Trials),
		CaptureTargetLogs: boolOr(f.Defaults.CaptureTargetLogs, false),
	}

	policy, err := resolvePolicy(scenario, f.Defaults, resolved.Target.Kind)
	if err != nil {
		return nil, fmt.Errorf("scenario %q: %w", name, err)
	}
	resolved.Policy = policy
	resolved.Termination = resolveTermination(scenario.Termination, policy.GracePeriod)

	if err := policy.Validate(); err != nil {
		return nil, fmt.Errorf("scenario %q: %w", name, err)
	}
	return resolved, nil
}

func resolveTarget(spec TargetSpec) Target {
	target := Target{Command: spec.Command, PID: spec.PID, Docker: spec.Docker}

	switch {
	case len(spec.Command) > 0:
		target.Kind = analyze.TargetCommand
		target.Label = strings.Join(spec.Command, " ")
	case spec.Docker != "":
		target.Kind = analyze.TargetDocker
		target.Label = "docker(" + spec.Docker + ")"
	default:
		target.Kind = analyze.TargetProcess
		target.Label = fmt.Sprintf("pid(%d)", spec.PID)
	}

	if spec.Ready != nil {
		target.Ready = Ready{
			URL:     spec.Ready.URL,
			Port:    spec.Ready.Port,
			Timeout: durationOr(spec.Ready.Timeout, 30*time.Second),
		}
	}
	return target
}

func resolveProbe(spec ProbeSpec) Probe {
	probe := Probe{
		ReadinessURL: spec.ReadinessURL,
		SlowURL:      spec.SlowURL,
		Insecure:     spec.Insecure,
		Requests:     make([]Request, 0, len(spec.Requests)),
	}

	for i, req := range spec.Requests {
		method := req.Method
		if method == "" {
			method = DefaultMethod
		}
		name := req.Name
		if name == "" {
			name = fmt.Sprintf("request-%d", i+1)
		}
		// A single unweighted request still needs a weight for selection.
		weight := req.Weight
		if weight == 0 && len(spec.Requests) == 1 {
			weight = 1
		}

		probe.Requests = append(probe.Requests, Request{
			Name:     name,
			Weight:   weight,
			URL:      req.URL,
			Method:   method,
			Headers:  req.Headers,
			Body:     req.Body,
			BodyFile: req.BodyFile,
		})
	}
	return probe
}

func resolveTraffic(spec LoadSpec, defaults Defaults) Traffic {
	load := Traffic{
		MaxRPS:         floatOr(spec.MaxRPS, DefaultMaxRPS),
		ConcurrencyCap: intOr(DefaultConcurrencyCap, spec.ConcurrencyCap),
		Warmup:         durationOr(spec.Warmup, DefaultWarmup),
		Steady:         durationOr(spec.Steady, DefaultSteady),
		RequestTimeout: durationOr(spec.RequestTimeout, DefaultRequestTimeout),
		KeepAlive:      boolOr(spec.KeepAlive, true),
	}
	if spec.Seed != nil {
		load.Seed = *spec.Seed
	}

	if spec.RPS != nil {
		load.RPS = *spec.RPS
		load.Calibrate = false
		return load
	}

	load.Calibrate = true
	load.EnsureInFlight = intOr(DefaultEnsureInFlight, spec.EnsureInFlight, defaults.EnsureInFlight)
	return load
}

func resolveTermination(spec TerminationSpec, grace time.Duration) Termination {
	signal := spec.Signal
	if signal == "" {
		signal = DefaultSignal
	}

	return Termination{
		Signal:         signal,
		PreStopSleep:   durationOr(spec.PreStopSleep, 0),
		GracePeriod:    grace,
		EnforceSigkill: boolOr(spec.EnforceSigkill, true),
	}
}

func resolvePolicy(scenario *Scenario, defaults Defaults, kind analyze.TargetKind) (analyze.Policy, error) {
	name := scenario.Termination.Profile
	if name == "" {
		name = defaults.Profile
	}
	if name == "" {
		name = string(analyze.ProfileAuto)
	}

	profile := analyze.Profile(name).Resolve(kind)
	policy, err := analyze.PolicyFor(profile)
	if err != nil {
		return analyze.Policy{}, err
	}

	if grace := firstDuration(scenario.Termination.GracePeriod, defaults.GracePeriod); grace != nil {
		policy.GracePeriod = *grace
	}
	if scenario.Termination.AcceptWindow != nil {
		policy.AcceptWindow = scenario.Termination.AcceptWindow.Duration()
	}

	gate := scenario.Gate
	if gate.MaxInFlightDropPct != nil {
		policy.MaxInFlightDropPct = *gate.MaxInFlightDropPct
	}
	if gate.MaxShutdownTime != nil {
		d := gate.MaxShutdownTime.Duration()
		policy.MaxShutdownTime = &d
	}
	if gate.MinScore != nil {
		score := *gate.MinScore
		policy.MinScore = &score
	}

	// Ignore is applied before fail_on so that an id in both, which validation
	// already rejects, could never end up silently suppressed.
	for _, id := range sortedCopy(gate.Ignore) {
		policy = policy.WithSeverity(analyze.SignatureID(id), schema.SeverityInfo)
	}
	for _, id := range sortedCopy(gate.FailOn) {
		policy = policy.WithSeverity(analyze.SignatureID(id), schema.SeverityError)
	}

	return policy, nil
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func intOr(fallback int, values ...*int) int {
	for _, v := range values {
		if v != nil {
			return *v
		}
	}
	return fallback
}

func firstDuration(values ...*Duration) *time.Duration {
	for _, v := range values {
		if v != nil {
			d := v.Duration()
			return &d
		}
	}
	return nil
}

func durationOr(v *Duration, fallback time.Duration) time.Duration {
	if v == nil {
		return fallback
	}
	return v.Duration()
}

func floatOr(v *float64, fallback float64) float64 {
	if v == nil {
		return fallback
	}
	return *v
}

func boolOr(v *bool, fallback bool) bool {
	if v == nil {
		return fallback
	}
	return *v
}
