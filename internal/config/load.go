package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/shutdowncheck/shutdowncheck/internal/analyze"
	"github.com/shutdowncheck/shutdowncheck/internal/target"
)

// Problem is one validation failure, located by its path in the config file.
type Problem struct {
	Path string
	Msg  string
}

func (p Problem) String() string { return p.Path + ": " + p.Msg }

// Problems is a set of validation failures. Reporting them together means a
// user fixes one config file once, instead of rerunning to discover the next
// mistake.
type Problems []Problem

func (p Problems) Error() string {
	if len(p) == 1 {
		return "invalid configuration: " + p[0].String()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "invalid configuration (%d problems):", len(p))
	for _, problem := range p {
		b.WriteString("\n  - " + problem.String())
	}
	return b.String()
}

func (p *Problems) add(path, format string, args ...any) {
	*p = append(*p, Problem{Path: path, Msg: fmt.Sprintf(format, args...)})
}

// ErrNoScenarios reports a config file that defines nothing to run.
var ErrNoScenarios = errors.New("config defines no scenarios")

// Load reads and validates a config file.
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- the path is supplied by the operator running the tool
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	file, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return file, nil
}

// Parse decodes and validates config bytes.
//
// Unknown fields are rejected rather than ignored: a silently-misspelled gate
// threshold would leave a CI job reporting success while checking nothing.
func Parse(data []byte) (*File, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var file File
	if err := dec.Decode(&file); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, ErrNoScenarios
		}
		return nil, fmt.Errorf("parse yaml: %w", err)
	}

	if err := file.Validate(); err != nil {
		return nil, err
	}
	return &file, nil
}

// Validate checks the whole file, gathering every problem it can find.
func (f *File) Validate() error {
	var problems Problems

	if f.Version == 0 {
		problems.add("version", "missing; add `version: %d`", SchemaVersion)
	} else if f.Version != SchemaVersion {
		problems.add("version", "unsupported version %d; this build understands version %d", f.Version, SchemaVersion)
	}

	if f.Defaults.Profile != "" && !analyze.Profile(f.Defaults.Profile).Valid() {
		problems.add("defaults.profile", "unknown profile %q; valid values are %s",
			f.Defaults.Profile, profileList())
	}
	validatePositiveInt(&problems, "defaults.ensure_in_flight", f.Defaults.EnsureInFlight)
	validatePositiveInt(&problems, "defaults.trials", f.Defaults.Trials)
	validatePositiveDuration(&problems, "defaults.grace_period", f.Defaults.GracePeriod)

	if len(f.Scenarios) == 0 {
		problems.add("scenarios", "no scenarios defined; add at least one under `scenarios:`")
	}

	for _, name := range sortedKeys(f.Scenarios) {
		path := "scenarios." + name
		scenario := f.Scenarios[name]
		if scenario == nil {
			// `scenarios:\n  api:` with no body decodes to a nil entry.
			problems.add(path, "scenario is empty; it needs at least `target` and `probe`")
			continue
		}
		scenario.validate(path, &problems)
	}

	if len(problems) > 0 {
		return problems
	}
	return nil
}

func (s *Scenario) validate(path string, problems *Problems) {
	s.Target.validate(path+".target", problems)
	s.Probe.validate(path+".probe", problems)
	s.Load.validate(path+".load", problems)
	s.Termination.validate(path+".termination", problems)
	s.Gate.validate(path+".gate", problems)
}

func (t TargetSpec) validate(path string, problems *Problems) {
	set := 0
	if len(t.Command) > 0 {
		set++
	}
	if t.PID != 0 {
		set++
	}
	if t.Docker != "" {
		set++
	}

	switch {
	case set == 0:
		problems.add(path, "no target selected; set exactly one of `command`, `pid` or `docker`")
	case set > 1:
		problems.add(path, "several targets selected; set exactly one of `command`, `pid` or `docker`")
	}

	if t.PID < 0 {
		problems.add(path+".pid", "must be positive, got %d", t.PID)
	}
	if t.PID == 1 {
		problems.add(path+".pid", "refusing to target PID 1; signalling the init process would take down the host or container")
	}
	if t.Docker != "" && !validContainerRef(t.Docker) {
		problems.add(path+".docker", "%q is not a valid container name or id", t.Docker)
	}

	if t.Ready != nil {
		if t.Ready.URL == "" && t.Ready.Port == 0 {
			problems.add(path+".ready", "set `url` or `port` so the tool knows when the target has started")
		}
		if t.Ready.URL != "" {
			validateHTTPURL(problems, path+".ready.url", t.Ready.URL)
		}
		if t.Ready.Port < 0 || t.Ready.Port > 65535 {
			problems.add(path+".ready.port", "must be between 1 and 65535, got %d", t.Ready.Port)
		}
		validatePositiveDuration(problems, path+".ready.timeout", t.Ready.Timeout)
	}
}

func (p ProbeSpec) validate(path string, problems *Problems) {
	if len(p.Requests) == 0 {
		problems.add(path+".requests", "no requests defined; add at least one with a `url`")
		return
	}

	totalWeight := 0
	for i, req := range p.Requests {
		reqPath := fmt.Sprintf("%s.requests[%d]", path, i)

		if req.URL == "" {
			problems.add(reqPath+".url", "missing")
		} else {
			validateHTTPURL(problems, reqPath+".url", req.URL)
		}
		if req.Weight < 0 {
			problems.add(reqPath+".weight", "must not be negative, got %d", req.Weight)
		}
		totalWeight += req.Weight

		if req.Body != "" && req.BodyFile != "" {
			problems.add(reqPath, "set either `body` or `body_file`, not both")
		}
		if req.Method != "" && strings.ToUpper(req.Method) != req.Method {
			problems.add(reqPath+".method", "must be upper case, got %q", req.Method)
		}
		for name := range req.Headers {
			if strings.TrimSpace(name) == "" {
				problems.add(reqPath+".headers", "header names must not be blank")
			}
		}
	}

	if len(p.Requests) > 1 && totalWeight == 0 {
		problems.add(path+".requests", "every weight is zero, so no request would ever be sent")
	}

	if p.ReadinessURL != "" {
		validateHTTPURL(problems, path+".readiness_url", p.ReadinessURL)
	}
	if p.SlowURL != "" {
		validateHTTPURL(problems, path+".slow_url", p.SlowURL)
	}
}

func (l LoadSpec) validate(path string, problems *Problems) {
	if l.RPS != nil && l.EnsureInFlight != nil {
		problems.add(path, "set either `rps` or `ensure_in_flight`; a fixed rate disables calibration, so both together are contradictory")
	}
	if l.RPS != nil && *l.RPS <= 0 {
		problems.add(path+".rps", "must be positive, got %v", *l.RPS)
	}
	if l.MaxRPS != nil && *l.MaxRPS <= 0 {
		problems.add(path+".max_rps", "must be positive, got %v", *l.MaxRPS)
	}
	if l.RPS != nil && l.MaxRPS != nil && *l.RPS > *l.MaxRPS {
		problems.add(path+".rps", "%v exceeds max_rps %v", *l.RPS, *l.MaxRPS)
	}

	validatePositiveInt(problems, path+".ensure_in_flight", l.EnsureInFlight)
	validatePositiveInt(problems, path+".concurrency_cap", l.ConcurrencyCap)
	validatePositiveDuration(problems, path+".warmup", l.Warmup)
	validatePositiveDuration(problems, path+".steady", l.Steady)
	validatePositiveDuration(problems, path+".request_timeout", l.RequestTimeout)
}

func (t TerminationSpec) validate(path string, problems *Problems) {
	if t.Signal != "" && !validSignal(t.Signal) {
		problems.add(path+".signal", "unsupported signal %q; use TERM, INT or QUIT", t.Signal)
	}
	if t.Profile != "" && !analyze.Profile(t.Profile).Valid() {
		problems.add(path+".profile", "unknown profile %q; valid values are %s", t.Profile, profileList())
	}
	validatePositiveDuration(problems, path+".prestop_sleep", t.PreStopSleep)
	validatePositiveDuration(problems, path+".grace_period", t.GracePeriod)

	if t.AcceptWindow != nil && t.GracePeriod != nil && *t.AcceptWindow >= *t.GracePeriod {
		problems.add(path+".accept_window",
			"%s must be shorter than the grace period %s, otherwise the service would be required to keep accepting traffic until it is killed",
			t.AcceptWindow, t.GracePeriod)
	}
}

func (g GateSpec) validate(path string, problems *Problems) {
	if g.MaxInFlightDropPct != nil && (*g.MaxInFlightDropPct < 0 || *g.MaxInFlightDropPct > 100) {
		problems.add(path+".max_inflight_drop_pct", "must be between 0 and 100, got %v", *g.MaxInFlightDropPct)
	}
	if g.MinScore != nil && (*g.MinScore < 0 || *g.MinScore > 100) {
		problems.add(path+".min_score", "must be between 0 and 100, got %d", *g.MinScore)
	}
	validatePositiveDuration(problems, path+".max_shutdown_time", g.MaxShutdownTime)
	validatePositiveInt(problems, path+".trials", g.Trials)

	// Typos here are dangerous: an unrecognised id in `ignore` would silently
	// fail to suppress anything, and in `fail_on` would silently fail to gate.
	validateSignatureIDs(problems, path+".fail_on", g.FailOn)
	validateSignatureIDs(problems, path+".ignore", g.Ignore)

	failing := map[string]bool{}
	for _, id := range g.FailOn {
		failing[id] = true
	}
	for _, id := range g.Ignore {
		if failing[id] {
			problems.add(path, "%s appears in both `fail_on` and `ignore`", id)
		}
	}
}

func validateSignatureIDs(problems *Problems, path string, ids []string) {
	for _, id := range ids {
		if _, ok := analyze.Lookup(analyze.SignatureID(id)); !ok {
			problems.add(path, "unknown signature %q; see `shutdowncheck explain` for the catalogue", id)
		}
	}
}

func validateHTTPURL(problems *Problems, path, raw string) {
	parsed, err := url.Parse(raw)
	if err != nil {
		problems.add(path, "%q is not a valid URL: %v", raw, err)
		return
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		problems.add(path, "%q must use http or https", raw)
		return
	}
	if parsed.Host == "" {
		problems.add(path, "%q has no host", raw)
	}
}

func validatePositiveInt(problems *Problems, path string, v *int) {
	if v != nil && *v <= 0 {
		problems.add(path, "must be positive, got %d", *v)
	}
}

func validatePositiveDuration(problems *Problems, path string, d *Duration) {
	if d != nil && d.Duration() <= 0 {
		problems.add(path, "must be positive, got %s", d)
	}
}

func validSignal(s string) bool {
	switch s {
	case "TERM", "INT", "QUIT":
		return true
	default:
		return false
	}
}

// validContainerRef restricts container references to what Docker itself
// allows. The value reaches a subprocess argv, so the rule is owned by the
// package that builds that argv rather than duplicated here.
func validContainerRef(s string) bool { return target.ValidContainerRef(s) }

func profileList() string {
	names := make([]string, 0, len(analyze.Profiles()))
	for _, p := range analyze.Profiles() {
		names = append(names, string(p))
	}
	return strings.Join(names, ", ")
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
