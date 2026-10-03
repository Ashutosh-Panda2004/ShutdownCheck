package cli

import (
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/analyze"
	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/config"
	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/probe"
	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/redact"
	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/remediate"
	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/report"
	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/target"
	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/timeline"
	"github.com/Ashutosh-Panda2004/ShutdownCheck/pkg/schema"
)

func (f *runFlags) applyTargetOverride(r *config.Resolved) (bool, error) {
	selected := 0
	for _, set := range []bool{len(f.argv) > 0, f.set["pid"], f.set["docker"]} {
		if set {
			selected++
		}
	}
	if selected > 1 {
		return false, usagef("choose exactly one of `-- <command>`, --pid and --docker")
	}

	changed := selected == 1
	switch {
	case len(f.argv) > 0:
		r.Target = config.Target{Kind: analyze.TargetCommand, Command: f.argv, Label: redact.Argv(f.argv)}
	case f.set["docker"]:
		r.Target = config.Target{Kind: analyze.TargetDocker, Docker: f.docker, Label: "docker(" + f.docker + ")"}
	case f.set["pid"]:
		r.Target = config.Target{Kind: analyze.TargetProcess, PID: f.pid, Label: fmt.Sprintf("pid(%d)", f.pid)}
	}

	if f.set["allow-unsafe-pid"] {
		if r.Target.Kind != analyze.TargetProcess {
			return false, usagef("--allow-unsafe-pid only applies to --pid targets")
		}
		r.Target.AllowUnsafePID = f.allowUnsafePID
	}
	return changed, nil
}

func reprofile(r *config.Resolved, profile analyze.Profile) error {
	old := r.Policy
	next, err := analyze.PolicyFor(profile)
	if err != nil {
		return usagef("%w", err)
	}

	// Gates are not profile defaults, so a profile override must not erase them.
	next.MaxInFlightDropPct = old.MaxInFlightDropPct
	next.MaxShutdownTime = old.MaxShutdownTime
	next.MinScore = old.MinScore

	if r.PolicyOverrides.GracePeriod {
		next.GracePeriod = old.GracePeriod
	}
	if r.PolicyOverrides.AcceptWindow {
		next.AcceptWindow = old.AcceptWindow
	}
	for id, severity := range r.PolicyOverrides.Severities {
		next = next.WithSeverity(id, severity)
	}

	r.Policy = next
	r.Termination.GracePeriod = next.GracePeriod
	return nil
}

func (f *runFlags) applyProbeOverrides(r *config.Resolved) error {
	if len(r.Probe.Requests) == 0 {
		return usagef("no requests configured")
	}
	request := &r.Probe.Requests[0]

	if f.set["url"] {
		request.URL = f.url
	}
	if f.set["method"] {
		request.Method = strings.ToUpper(strings.TrimSpace(f.method))
	}
	if f.set["header"] {
		if request.Headers == nil {
			request.Headers = map[string]string{}
		}
		for name, value := range f.headers {
			request.Headers[name] = value
		}
	}
	if f.set["body"] && f.set["body-file"] {
		return usagef("set either --body or --body-file, not both")
	}
	if f.set["body"] {
		if len(f.body) > MaxRequestBodyBytes {
			return usagef("request body exceeds the %d-byte limit", MaxRequestBodyBytes)
		}
		request.Body, request.BodyFile = f.body, ""
	}
	if f.set["body-file"] {
		body, err := readRequestBody(f.bodyFile)
		if err != nil {
			return usagef("read --body-file: %w", err)
		}
		request.Body, request.BodyFile = string(body), ""
	}
	if f.set["readiness-url"] {
		r.Probe.ReadinessURL = f.readinessURL
	}
	if f.set["slow-url"] {
		r.Probe.SlowURL = f.slowURL
	}
	if f.set["insecure"] {
		r.Probe.Insecure = f.insecure
	}
	return nil
}

func (f *runFlags) applyLoadOverrides(r *config.Resolved) {
	if f.set["ensure-in-flight"] {
		r.Traffic.EnsureInFlight = f.ensureInFlight
		r.Traffic.RPS = 0
		r.Traffic.Calibrate = true
	}
	if f.set["rps"] {
		r.Traffic.RPS = f.rps
		r.Traffic.EnsureInFlight = 0
		r.Traffic.Calibrate = false
	}
	if f.set["max-rps"] {
		r.Traffic.MaxRPS = f.maxRPS
	}
	if f.set["concurrency-cap"] {
		r.Traffic.ConcurrencyCap = f.concurrencyCap
	}
	if f.set["warmup"] {
		r.Traffic.Warmup = f.warmup
	}
	if f.set["steady"] {
		r.Traffic.Steady = f.steady
	}
	if f.set["request-timeout"] {
		r.Traffic.RequestTimeout = f.requestTimeout
	}
	if f.set["keep-alive"] {
		r.Traffic.KeepAlive = f.keepAlive
	}
}

func (f *runFlags) applyTerminationOverrides(r *config.Resolved) {
	if f.set["signal"] {
		r.Termination.Signal = strings.ToUpper(f.signalName)
	}
	if f.set["prestop-sleep"] {
		r.Termination.PreStopSleep = f.preStopSleep
	}
	if f.set["grace-period"] {
		r.Policy.GracePeriod = f.gracePeriod
		r.Termination.GracePeriod = f.gracePeriod
	}
	if f.set["enforce-sigkill"] {
		r.Termination.EnforceSigkill = f.enforceSigkill
	}
	if f.set["accept-window"] {
		r.Policy.AcceptWindow = f.acceptWindow
	}
}

func (f *runFlags) applyGateOverrides(r *config.Resolved) {
	if f.set["max-inflight-drop-pct"] {
		r.Policy.MaxInFlightDropPct = f.maxDropPct
	}
	if f.set["max-shutdown-time"] {
		d := f.maxShutdownTime
		r.Policy.MaxShutdownTime = &d
	}
	if f.set["min-score"] {
		score := f.minScore
		r.Policy.MinScore = &score
	}
	for _, id := range f.ignore {
		r.Policy = r.Policy.WithSeverity(analyze.SignatureID(id), schema.SeverityInfo)
	}
	for _, id := range f.failOn {
		r.Policy = r.Policy.WithSeverity(analyze.SignatureID(id), schema.SeverityError)
	}
}

func (f *runFlags) validateResolved(r *config.Resolved) error {
	if math.IsNaN(r.Traffic.RPS) || math.IsInf(r.Traffic.RPS, 0) ||
		math.IsNaN(r.Traffic.MaxRPS) || math.IsInf(r.Traffic.MaxRPS, 0) {
		return usagef("--rps and --max-rps must be finite numbers")
	}
	if f.set["rps"] && f.set["ensure-in-flight"] {
		return usagef("set either --rps or --ensure-in-flight; a fixed rate disables calibration")
	}
	if r.Traffic.Calibrate && r.Traffic.EnsureInFlight <= 0 {
		return usagef("--ensure-in-flight must be positive, got %d", r.Traffic.EnsureInFlight)
	}
	for _, request := range r.Probe.Requests {
		if !probe.ValidMethod(request.Method) {
			return usagef("request method must be a valid HTTP token")
		}
		for name, value := range request.Headers {
			if !probe.ValidHeaderName(name) || !probe.ValidHeaderValue(value) {
				return usagef("request contains an invalid HTTP header")
			}
		}
	}
	if !r.Traffic.Calibrate && r.Traffic.RPS <= 0 {
		return usagef("--rps must be positive, got %v", r.Traffic.RPS)
	}
	if !r.Traffic.Calibrate && r.Probe.SlowURL != "" {
		return usagef("--slow-url requires --ensure-in-flight calibration and cannot be combined with --rps")
	}
	if r.Traffic.MaxRPS <= 0 || r.Traffic.ConcurrencyCap <= 0 {
		return usagef("--max-rps and --concurrency-cap must be positive")
	}
	if r.Traffic.RPS > config.MaxRPS || r.Traffic.MaxRPS > config.MaxRPS {
		return usagef("--rps and --max-rps must not exceed %v", config.MaxRPS)
	}
	if r.Traffic.ConcurrencyCap > config.MaxConcurrencyCap {
		return usagef("--concurrency-cap must not exceed %d", config.MaxConcurrencyCap)
	}
	if !r.Traffic.Calibrate && r.Traffic.RPS > r.Traffic.MaxRPS {
		return usagef("--rps %v exceeds --max-rps %v", r.Traffic.RPS, r.Traffic.MaxRPS)
	}
	if r.Traffic.Warmup <= 0 || r.Traffic.Steady <= 0 || r.Traffic.RequestTimeout <= 0 {
		return usagef("--warmup, --steady and --request-timeout must be positive")
	}
	for name, duration := range map[string]time.Duration{
		"--warmup": r.Traffic.Warmup, "--steady": r.Traffic.Steady,
		"--request-timeout": r.Traffic.RequestTimeout,
		"--prestop-sleep":   r.Termination.PreStopSleep,
		"--grace-period":    r.Termination.GracePeriod,
		"--accept-window":   r.Policy.AcceptWindow,
	} {
		if duration > config.MaxOperationalDuration {
			return usagef("%s must not exceed %s", name, config.MaxOperationalDuration)
		}
	}
	if r.Policy.MaxShutdownTime != nil && *r.Policy.MaxShutdownTime > config.MaxOperationalDuration {
		return usagef("--max-shutdown-time must not exceed %s", config.MaxOperationalDuration)
	}
	if r.Termination.PreStopSleep < 0 {
		return usagef("--prestop-sleep cannot be negative")
	}
	signal := target.Signal(r.Termination.Signal)
	if !signal.Graceful() {
		return usagef("unsupported graceful signal %q; use TERM, INT or QUIT", r.Termination.Signal)
	}
	if r.Trials < 1 {
		return usagef("--trials must be positive, got %d", r.Trials)
	}
	if r.Trials > config.MaxTrials {
		return usagef("--trials must not exceed %d", config.MaxTrials)
	}
	if f.timeout < 0 {
		return usagef("--timeout cannot be negative")
	}
	if f.timeout > 30*24*time.Hour {
		return usagef("--timeout must not exceed %s", 30*24*time.Hour)
	}
	if f.maxRecords <= 0 {
		return usagef("--max-records must be positive")
	}
	if f.maxRecords > timeline.MaxRecordLimit {
		return usagef("--max-records must not exceed %d", timeline.MaxRecordLimit)
	}
	if f.observeInterval < 5*time.Millisecond {
		return usagef("--observe-interval must be at least 5ms")
	}
	if f.width < 0 {
		return usagef("--width cannot be negative")
	}
	if f.width > report.MaxWidth {
		return usagef("--width must not exceed %d", report.MaxWidth)
	}
	if f.output != "" && f.badge != "" && samePath(f.output, f.badge) {
		return usagef("--output and --badge must name different files")
	}
	if !report.Format(f.format).Valid() {
		return usagef("unknown format %q; valid values are %v", f.format, report.Formats())
	}
	if f.stack != "" && !remediate.Stack(f.stack).Valid() {
		return usagef("unknown stack %q; valid values are %v", f.stack, remediate.Stacks())
	}

	seen := map[analyze.SignatureID]string{}
	for _, item := range []struct {
		name string
		ids  idList
	}{{"--ignore", f.ignore}, {"--fail-on", f.failOn}} {
		for _, raw := range item.ids {
			id := analyze.SignatureID(raw)
			if _, ok := analyze.Lookup(id); !ok {
				return usagef("unknown signature %q in %s", id, item.name)
			}
			if previous, ok := seen[id]; ok && previous != item.name {
				return usagef("signature %s appears in both --ignore and --fail-on", id)
			}
			seen[id] = item.name
		}
	}

	allowPathOnly := r.Target.Kind == analyze.TargetDocker
	if err := validateRunURL("request URL", r.Probe.Requests[0].URL, allowPathOnly); err != nil {
		return err
	}
	for _, request := range r.Probe.Requests[1:] {
		if err := validateRunURL("request URL", request.URL, allowPathOnly); err != nil {
			return err
		}
	}
	for name, raw := range map[string]string{
		"target readiness URL": r.Target.Ready.URL,
		"readiness URL":        r.Probe.ReadinessURL,
		"slow URL":             r.Probe.SlowURL,
	} {
		if raw != "" {
			if err := validateRunURL(name, raw, allowPathOnly); err != nil {
				return err
			}
		}
	}
	if f.containerPort != "" && r.Target.Kind != analyze.TargetDocker {
		return usagef("--container-port only applies to --docker targets")
	}
	if f.containerPort != "" && !target.ValidContainerPort(f.containerPort) {
		return usagef("--container-port must be a TCP port between 1 and 65535")
	}
	if r.Target.Kind == analyze.TargetDocker && !target.ValidContainerRef(r.Target.Docker) {
		return usagef("%q is not a valid container name or id", r.Target.Docker)
	}
	if r.Target.Kind == analyze.TargetProcess {
		if err := target.ValidatePID(r.Target.PID, os.Getpid(), r.Target.AllowUnsafePID); err != nil {
			return usagef("%w", err)
		}
	}
	// A profile's default accept window must not collide with a grace period
	// the operator did choose: --profile kubernetes --grace-period 5s would
	// otherwise be a usage error (the untouched default window is also 5s)
	// even though the operator never set a window. Shrink the default to
	// half the grace period. An explicitly configured window, from a flag or
	// a file, is never rewritten; the Validate below still rejects one that
	// does not fit.
	if !f.set["accept-window"] && !r.PolicyOverrides.AcceptWindow &&
		r.Policy.GracePeriod > 0 && r.Policy.AcceptWindow >= r.Policy.GracePeriod {
		r.Policy.AcceptWindow = r.Policy.GracePeriod / 2
	}
	if err := r.Policy.Validate(); err != nil {
		return usagef("%w", err)
	}
	return nil
}

func samePath(a, b string) bool {
	a, errA := filepath.Abs(filepath.Clean(a))
	b, errB := filepath.Abs(filepath.Clean(b))
	return errA == nil && errB == nil && strings.EqualFold(a, b)
}

func validateRunURL(name, raw string, allowPathOnly bool) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return usagef("%s %q is invalid: %s", name, redact.URL(raw), redact.Message(err.Error()))
	}
	if allowPathOnly && isPathOnlyURL(raw) {
		return nil
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return usagef("%s %q must be an absolute http or https URL", name, redact.URL(raw))
	}
	return nil
}

func isPathOnlyURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "" && parsed.Host == "" &&
		strings.HasPrefix(parsed.Path, "/") && !strings.HasPrefix(raw, "//")
}
