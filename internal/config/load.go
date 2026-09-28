package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/analyze"
	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/probe"
	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/redact"
	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/target"
)

// Configuration limits prevent a small YAML file from amplifying into
// excessive validation output or per-dispatch scheduler work.
const (
	MaxConfigBytes        = 4 << 20
	MaxScenarios          = 100
	MaxRequestDefinitions = 100
	MaxHeadersPerRequest  = 100
	MaxCommandArgs        = 1_024
	MaxSignatureOverrides = 100
	maxValidationProblems = 100
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
	if len(*p) >= maxValidationProblems {
		return
	}
	if len(*p) == maxValidationProblems-1 {
		*p = append(*p, Problem{Path: "configuration", Msg: "additional validation problems omitted"})
		return
	}
	*p = append(*p, Problem{Path: path, Msg: fmt.Sprintf(format, args...)})
}

// ErrNoScenarios reports a config file that defines nothing to run.
var ErrNoScenarios = errors.New("config defines no scenarios")

// Load reads and validates a config file.
func Load(path string) (*File, error) {
	f, err := os.Open(path) // #nosec G304 -- the path is supplied by the operator running the tool
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, MaxConfigBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	if len(data) > MaxConfigBytes {
		return nil, fmt.Errorf("read config %s: file exceeds the %d-byte limit", path, MaxConfigBytes)
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
	if len(data) > MaxConfigBytes {
		return nil, fmt.Errorf("configuration exceeds the %d-byte limit", MaxConfigBytes)
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var file File
	if err := dec.Decode(&file); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, ErrNoScenarios
		}
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("parse yaml: multiple documents are not allowed")
		}
		return nil, fmt.Errorf("parse trailing yaml: %w", err)
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
	if f.Defaults.Trials != nil && *f.Defaults.Trials > MaxTrials {
		problems.add("defaults.trials", "must not exceed %d, got %d", MaxTrials, *f.Defaults.Trials)
	}
	validatePositiveDuration(&problems, "defaults.grace_period", f.Defaults.GracePeriod)

	if len(f.Scenarios) == 0 {
		problems.add("scenarios", "no scenarios defined; add at least one under `scenarios:`")
	}
	if len(f.Scenarios) > MaxScenarios {
		problems.add("scenarios", "must not define more than %d scenarios, got %d", MaxScenarios, len(f.Scenarios))
	}

	names := sortedKeys(f.Scenarios)
	if len(names) > MaxScenarios {
		names = names[:MaxScenarios]
	}
	for _, name := range names {
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
	s.Probe.validate(path+".probe", problems, s.Target.Docker != "")
	s.Load.validate(path+".load", problems)
	if s.Load.RPS != nil && s.Probe.SlowURL != "" {
		problems.add(path+".probe.slow_url", "requires ensure_in_flight calibration and cannot be combined with fixed rps")
	}
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
	if len(t.Command) > MaxCommandArgs {
		problems.add(path+".command", "must not contain more than %d arguments, got %d", MaxCommandArgs, len(t.Command))
	}
	if len(t.Command) > 0 && strings.TrimSpace(t.Command[0]) == "" {
		problems.add(path+".command[0]", "executable must not be blank")
	}
	if t.Docker != "" && !validContainerRef(t.Docker) {
		problems.add(path+".docker", "%q is not a valid container name or id", t.Docker)
	}

	if t.Ready != nil {
		if t.Ready.URL == "" && t.Ready.Port == 0 {
			problems.add(path+".ready", "set `url` or `port` so the tool knows when the target has started")
		}
		if t.Ready.URL != "" && t.Ready.Port != 0 {
			problems.add(path+".ready", "set either `url` or `port`, not both")
		}
		if t.Ready.URL != "" {
			validateHTTPURL(problems, path+".ready.url", t.Ready.URL, t.Docker != "")
		}
		if t.Ready.Port < 0 || t.Ready.Port > 65535 {
			problems.add(path+".ready.port", "must be between 1 and 65535, got %d", t.Ready.Port)
		}
		validatePositiveDuration(problems, path+".ready.timeout", t.Ready.Timeout)
	}
}

func (p ProbeSpec) validate(path string, problems *Problems, allowPathOnly bool) {
	if len(p.Requests) == 0 {
		problems.add(path+".requests", "no requests defined; add at least one with a `url`")
		return
	}
	if len(p.Requests) > MaxRequestDefinitions {
		problems.add(path+".requests", "must not contain more than %d definitions, got %d", MaxRequestDefinitions, len(p.Requests))
	}

	hasPositiveWeight := false
	requests := p.Requests
	if len(requests) > MaxRequestDefinitions {
		requests = requests[:MaxRequestDefinitions]
	}
	for i, req := range requests {
		reqPath := fmt.Sprintf("%s.requests[%d]", path, i)

		if req.URL == "" {
			problems.add(reqPath+".url", "missing")
		} else {
			validateHTTPURL(problems, reqPath+".url", req.URL, allowPathOnly)
		}
		if req.Weight < 0 {
			problems.add(reqPath+".weight", "must not be negative, got %d", req.Weight)
		}
		if req.Weight > 0 {
			hasPositiveWeight = true
		}

		if req.Body != "" && req.BodyFile != "" {
			problems.add(reqPath, "set either `body` or `body_file`, not both")
		}
		if req.Method != "" {
			if strings.ToUpper(req.Method) != req.Method {
				problems.add(reqPath+".method", "must be upper case, got %q", redact.Text(req.Method))
			}
			if !probe.ValidMethod(req.Method) {
				problems.add(reqPath+".method", "must be a valid HTTP token")
			}
		}
		if len(req.Headers) > MaxHeadersPerRequest {
			problems.add(reqPath+".headers", "must not contain more than %d headers, got %d", MaxHeadersPerRequest, len(req.Headers))
		}
		headerNames := map[string]string{}
		headerKeys := sortedKeys(req.Headers)
		if len(headerKeys) > MaxHeadersPerRequest {
			headerKeys = headerKeys[:MaxHeadersPerRequest]
		}
		for _, name := range headerKeys {
			value := req.Headers[name]
			if !probe.ValidHeaderName(name) {
				problems.add(reqPath+".headers", "contains an invalid header name")
			}
			if !probe.ValidHeaderValue(value) {
				problems.add(reqPath+".headers", "header values must not contain control characters")
			}
			folded := strings.ToLower(name)
			if previous, exists := headerNames[folded]; exists {
				problems.add(reqPath+".headers", "contains duplicate names %q and %q that differ only by case", previous, name)
			} else {
				headerNames[folded] = name
			}
		}
	}

	if len(p.Requests) > 1 && !hasPositiveWeight {
		problems.add(path+".requests", "every weight is zero, so no request would ever be sent")
	}

	if p.ReadinessURL != "" {
		validateHTTPURL(problems, path+".readiness_url", p.ReadinessURL, allowPathOnly)
	}
	if p.SlowURL != "" {
		validateHTTPURL(problems, path+".slow_url", p.SlowURL, allowPathOnly)
	}
}

func (l LoadSpec) validate(path string, problems *Problems) {
	if l.RPS != nil && l.EnsureInFlight != nil {
		problems.add(path, "set either `rps` or `ensure_in_flight`; a fixed rate disables calibration, so both together are contradictory")
	}
	if l.RPS != nil && (!finite(*l.RPS) || *l.RPS <= 0) {
		problems.add(path+".rps", "must be positive, got %v", *l.RPS)
	}
	if l.MaxRPS != nil && (!finite(*l.MaxRPS) || *l.MaxRPS <= 0) {
		problems.add(path+".max_rps", "must be positive, got %v", *l.MaxRPS)
	}
	if l.RPS != nil && *l.RPS > MaxRPS {
		problems.add(path+".rps", "must not exceed %v, got %v", MaxRPS, *l.RPS)
	}
	if l.MaxRPS != nil && *l.MaxRPS > MaxRPS {
		problems.add(path+".max_rps", "must not exceed %v, got %v", MaxRPS, *l.MaxRPS)
	}
	if l.RPS != nil && l.MaxRPS != nil && *l.RPS > *l.MaxRPS {
		problems.add(path+".rps", "%v exceeds max_rps %v", *l.RPS, *l.MaxRPS)
	}

	validatePositiveInt(problems, path+".ensure_in_flight", l.EnsureInFlight)
	validatePositiveInt(problems, path+".concurrency_cap", l.ConcurrencyCap)
	if l.ConcurrencyCap != nil && *l.ConcurrencyCap > MaxConcurrencyCap {
		problems.add(path+".concurrency_cap", "must not exceed %d, got %d", MaxConcurrencyCap, *l.ConcurrencyCap)
	}
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
	if g.MaxInFlightDropPct != nil && (!finite(*g.MaxInFlightDropPct) || *g.MaxInFlightDropPct < 0 || *g.MaxInFlightDropPct > 100) {
		problems.add(path+".max_inflight_drop_pct", "must be between 0 and 100, got %v", *g.MaxInFlightDropPct)
	}
	if g.MinScore != nil && (*g.MinScore < 0 || *g.MinScore > 100) {
		problems.add(path+".min_score", "must be between 0 and 100, got %d", *g.MinScore)
	}
	validatePositiveDuration(problems, path+".max_shutdown_time", g.MaxShutdownTime)
	validatePositiveInt(problems, path+".trials", g.Trials)
	if g.Trials != nil && *g.Trials > MaxTrials {
		problems.add(path+".trials", "must not exceed %d, got %d", MaxTrials, *g.Trials)
	}
	if len(g.FailOn) > MaxSignatureOverrides {
		problems.add(path+".fail_on", "must not contain more than %d entries", MaxSignatureOverrides)
	}
	if len(g.Ignore) > MaxSignatureOverrides {
		problems.add(path+".ignore", "must not contain more than %d entries", MaxSignatureOverrides)
	}

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

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func validateSignatureIDs(problems *Problems, path string, ids []string) {
	if len(ids) > MaxSignatureOverrides {
		ids = ids[:MaxSignatureOverrides]
	}
	for _, id := range ids {
		if _, ok := analyze.Lookup(analyze.SignatureID(id)); !ok {
			problems.add(path, "unknown signature %q; see `shutdowncheck explain` for the catalogue", id)
		}
	}
}

func validateHTTPURL(problems *Problems, path, raw string, allowPathOnly ...bool) {
	parsed, err := url.Parse(raw)
	if err != nil {
		problems.add(path, "%q is not a valid URL: %s", redact.URL(raw), redact.Message(err.Error()))
		return
	}
	if len(allowPathOnly) > 0 && allowPathOnly[0] && parsed.Scheme == "" &&
		parsed.Host == "" && strings.HasPrefix(parsed.Path, "/") &&
		!strings.HasPrefix(raw, "//") {
		return
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		problems.add(path, "%q must use http or https", redact.URL(raw))
		return
	}
	if parsed.Host == "" {
		problems.add(path, "%q has no host", redact.URL(raw))
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
	if d != nil && d.Duration() > MaxOperationalDuration {
		problems.add(path, "must not exceed %s, got %s", MaxOperationalDuration, d)
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
