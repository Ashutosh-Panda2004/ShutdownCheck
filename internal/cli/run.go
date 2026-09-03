package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/shutdowncheck/shutdowncheck/internal/analyze"
	"github.com/shutdowncheck/shutdowncheck/internal/clock"
	"github.com/shutdowncheck/shutdowncheck/internal/config"
	"github.com/shutdowncheck/shutdowncheck/internal/load"
	"github.com/shutdowncheck/shutdowncheck/internal/probe"
	"github.com/shutdowncheck/shutdowncheck/internal/remediate"
	"github.com/shutdowncheck/shutdowncheck/internal/report"
	"github.com/shutdowncheck/shutdowncheck/internal/run"
	"github.com/shutdowncheck/shutdowncheck/internal/target"
	"github.com/shutdowncheck/shutdowncheck/internal/timeline"
	"github.com/shutdowncheck/shutdowncheck/pkg/schema"
)

type runFlags struct {
	fs  *flag.FlagSet
	set map[string]bool

	configPath string
	scenario   string

	pid    int
	docker string
	argv   []string

	url          string
	method       string
	headers      headerList
	body         string
	bodyFile     string
	readinessURL string
	insecure     bool

	ensureInFlight int
	rps            float64
	maxRPS         float64
	concurrencyCap int
	warmup         time.Duration
	steady         time.Duration
	requestTimeout time.Duration
	keepAlive      bool

	signalName     string
	preStopSleep   time.Duration
	gracePeriod    time.Duration
	enforceSigkill bool
	profile        string
	acceptWindow   time.Duration

	maxDropPct      float64
	maxShutdownTime time.Duration
	minScore        int
	failOn          idList
	ignore          idList
	trials          int

	format          string
	output          string
	badge           string
	noColor         bool
	noTimeline      bool
	width           int
	quiet           bool
	captureLogs     bool
	stack           string
	observeInterval time.Duration

	// lastTimeline is the evidence from the final trial, kept for the human and
	// NDJSON renderers which need more than the aggregated report.
	lastTimeline timeline.Timeline
}

func parseRunFlags(args []string, stderr io.Writer) (*runFlags, error) {
	f := &runFlags{fs: flag.NewFlagSet("run", flag.ContinueOnError), set: map[string]bool{}}
	f.fs.SetOutput(io.Discard)

	f.fs.StringVar(&f.configPath, "config", "", "configuration file (defaults to ./shutdowncheck.yaml when present)")
	f.fs.StringVar(&f.scenario, "scenario", "", "scenario to run from the configuration file")

	f.fs.IntVar(&f.pid, "pid", 0, "attach to an existing process by id")
	f.fs.StringVar(&f.docker, "docker", "", "target a running container by name or id")

	f.fs.StringVar(&f.url, "url", "", "endpoint to load (required)")
	f.fs.StringVar(&f.method, "method", "", "HTTP method (default GET)")
	f.fs.Var(&f.headers, "header", "request header as `Name: value` (repeatable)")
	f.fs.StringVar(&f.body, "body", "", "request body")
	f.fs.StringVar(&f.bodyFile, "body-file", "", "read the request body from a file")
	f.fs.StringVar(&f.readinessURL, "readiness-url", "", "readiness endpoint to poll (strongly recommended)")
	f.fs.BoolVar(&f.insecure, "insecure", false, "skip TLS certificate verification")

	f.fs.IntVar(&f.ensureInFlight, "ensure-in-flight", config.DefaultEnsureInFlight, "requests to hold in flight at the signal")
	f.fs.Float64Var(&f.rps, "rps", 0, "fixed request rate; disables calibration")
	f.fs.Float64Var(&f.maxRPS, "max-rps", config.DefaultMaxRPS, "ceiling for the calibrated rate")
	f.fs.IntVar(&f.concurrencyCap, "concurrency-cap", config.DefaultConcurrencyCap, "maximum open sockets")
	f.fs.DurationVar(&f.warmup, "warmup", config.DefaultWarmup, "warmup duration before calibration")
	f.fs.DurationVar(&f.steady, "steady", config.DefaultSteady, "steady traffic before the signal")
	f.fs.DurationVar(&f.requestTimeout, "request-timeout", config.DefaultRequestTimeout, "per-request timeout")
	f.fs.BoolVar(&f.keepAlive, "keep-alive", true, "reuse connections")

	f.fs.StringVar(&f.signalName, "signal", config.DefaultSignal, "signal to send: TERM, INT or QUIT")
	f.fs.DurationVar(&f.preStopSleep, "prestop-sleep", 0, "simulate a Kubernetes preStop hook")
	f.fs.DurationVar(&f.gracePeriod, "grace-period", 0, "grace period before SIGKILL (default depends on target)")
	f.fs.BoolVar(&f.enforceSigkill, "enforce-sigkill", true, "actually send SIGKILL at grace expiry")
	f.fs.StringVar(&f.profile, "profile", string(analyze.ProfileAuto), "shutdown profile: auto, standalone, strict, lame-duck, kubernetes, docker")
	f.fs.DurationVar(&f.acceptWindow, "accept-window", 0, "how long new connections may still be accepted")

	f.fs.Float64Var(&f.maxDropPct, "max-inflight-drop-pct", 0, "fail above this percentage of dropped in-flight requests")
	f.fs.DurationVar(&f.maxShutdownTime, "max-shutdown-time", 0, "fail if shutdown takes longer than this")
	f.fs.IntVar(&f.minScore, "min-score", 0, "fail below this score")
	f.fs.Var(&f.failOn, "fail-on", "promote signatures to error severity (comma-separated or repeated)")
	f.fs.Var(&f.ignore, "ignore", "demote signatures to informational (comma-separated or repeated)")
	f.fs.IntVar(&f.trials, "trials", 0, "repeat the experiment this many times")

	f.fs.StringVar(&f.format, "format", string(report.FormatHuman), "output format: human, json, junit, markdown, ndjson")
	f.fs.StringVar(&f.output, "output", "", "also write the report to this file")
	f.fs.StringVar(&f.badge, "badge", "", "write an SVG score badge to this file")
	f.fs.BoolVar(&f.noColor, "no-color", false, "disable coloured output")
	f.fs.BoolVar(&f.noTimeline, "no-timeline", false, "omit the timeline visualisation")
	f.fs.IntVar(&f.width, "width", 0, "terminal width for the report")
	f.fs.BoolVar(&f.quiet, "quiet", false, "suppress progress output")
	f.fs.BoolVar(&f.captureLogs, "capture-target-logs", false, "interleave target output into the timeline")
	f.fs.StringVar(&f.stack, "stack", "", "override framework detection for remediation advice")
	f.fs.DurationVar(&f.observeInterval, "observe-interval", 20*time.Millisecond, "readiness and listener polling cadence")

	f.fs.Usage = func() { printRunUsage(stderr) }

	if err := f.fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			printRunUsage(stderr)
			return nil, &usageError{err}
		}
		return nil, usagef("%w", err)
	}

	f.fs.Visit(func(fl *flag.Flag) { f.set[fl.Name] = true })
	f.argv = f.fs.Args()
	return f, nil
}

func printRunUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: shutdowncheck run [flags] [-- <command> [args...]]

Terminates a target under load and reports which stage of shutdown failed.

Target (choose exactly one):
  -- <command>            spawn and manage the process (recommended)
  --pid <n>               attach to an existing process
  --docker <name|id>      target a running container

Common flags:
  --url <url>             endpoint to load (required)
  --readiness-url <url>   readiness endpoint to poll
  --profile <name>        auto, standalone, strict, lame-duck, kubernetes, docker
  --grace-period <dur>    how long the orchestrator would wait before SIGKILL
  --trials <n>            repeat the experiment; 3 is recommended in CI
  --format <name>         human, json, junit, markdown, ndjson
  --config <path>         read settings from a configuration file

Profiles decide what "correct" means. Under kubernetes and lame-duck a service
must keep serving briefly after the signal, because de-registration is
asynchronous; under strict it must stop accepting immediately. Run
"shutdowncheck explain SC006" for why this matters.

Run "shutdowncheck run --help" to see every flag.

`)
}

func runCommand(args []string, stdout, stderr io.Writer) int {
	flags, err := parseRunFlags(args, stderr)
	if err != nil {
		return exitFor(err, stderr)
	}

	resolved, err := flags.resolve()
	if err != nil {
		return exitFor(err, stderr)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	result, err := executeTrials(ctx, flags, resolved, stderr)
	if err != nil {
		return exitFor(err, stderr)
	}

	if err := emit(flags, result, stdout); err != nil {
		return exitFor(err, stderr)
	}
	return result.Report.Verdict.ExitCode()
}

// resolve merges the configuration file with explicit flags. Flags always win,
// so a CI job can override one setting without restating the whole file.
func (f *runFlags) resolve() (*config.Resolved, error) {
	path := f.configPath
	if path == "" {
		if _, err := os.Stat(config.DefaultConfigPath); err == nil {
			path = config.DefaultConfigPath
		}
	}

	var resolved *config.Resolved
	if path != "" {
		file, err := config.Load(path)
		if err != nil {
			return nil, usagef("%w", err)
		}
		resolved, err = file.Resolve(f.scenario)
		if err != nil {
			return nil, usagef("%w", err)
		}
	} else {
		var err error
		if resolved, err = f.buildFromFlags(); err != nil {
			return nil, err
		}
	}

	if err := f.applyOverrides(resolved); err != nil {
		return nil, err
	}
	if err := resolved.Policy.Validate(); err != nil {
		return nil, usagef("%w", err)
	}
	return resolved, nil
}

func (f *runFlags) buildFromFlags() (*config.Resolved, error) {
	targets := 0
	for _, chosen := range []bool{len(f.argv) > 0, f.pid != 0, f.docker != ""} {
		if chosen {
			targets++
		}
	}
	switch {
	case targets == 0:
		return nil, usagef("no target given; pass `-- <command>`, --pid or --docker")
	case targets > 1:
		return nil, usagef("choose exactly one of `-- <command>`, --pid and --docker")
	}
	if f.url == "" {
		return nil, usagef("--url is required")
	}

	body := []byte(f.body)
	if f.bodyFile != "" {
		if f.body != "" {
			return nil, usagef("set either --body or --body-file, not both")
		}
		data, err := os.ReadFile(filepath.Clean(f.bodyFile)) // #nosec G304 -- operator-supplied path
		if err != nil {
			return nil, usagef("read --body-file: %w", err)
		}
		body = data
	}

	method := f.method
	if method == "" {
		method = config.DefaultMethod
	}

	resolved := &config.Resolved{
		Name: "cli",
		Probe: config.Probe{
			ReadinessURL: f.readinessURL,
			Insecure:     f.insecure,
			Requests: []config.Request{{
				Name: "request", Weight: 1, URL: f.url, Method: method,
				Headers: f.headers, Body: string(body),
			}},
		},
		Traffic: config.Traffic{
			Calibrate: f.rps <= 0, EnsureInFlight: f.ensureInFlight, RPS: f.rps,
			MaxRPS: f.maxRPS, ConcurrencyCap: f.concurrencyCap,
			Warmup: f.warmup, Steady: f.steady, RequestTimeout: f.requestTimeout,
			KeepAlive: f.keepAlive,
		},
		Termination: config.Termination{
			Signal: strings.ToUpper(f.signalName), PreStopSleep: f.preStopSleep,
			EnforceSigkill: f.enforceSigkill,
		},
		Trials:            1,
		CaptureTargetLogs: f.captureLogs,
	}

	switch {
	case len(f.argv) > 0:
		resolved.Target = config.Target{
			Kind: analyze.TargetCommand, Command: f.argv, Label: strings.Join(f.argv, " "),
		}
	case f.docker != "":
		resolved.Target = config.Target{
			Kind: analyze.TargetDocker, Docker: f.docker, Label: "docker(" + f.docker + ")",
		}
	default:
		resolved.Target = config.Target{
			Kind: analyze.TargetProcess, PID: f.pid, Label: fmt.Sprintf("pid(%d)", f.pid),
		}
	}

	profile := analyze.Profile(f.profile).Resolve(resolved.Target.Kind)
	policy, err := analyze.PolicyFor(profile)
	if err != nil {
		return nil, usagef("%w", err)
	}
	resolved.Policy = policy
	resolved.Termination.GracePeriod = policy.GracePeriod
	return resolved, nil
}

// applyOverrides layers explicitly-set flags over whatever the configuration
// produced. Only flags the user actually typed are applied, so a default can
// never silently overwrite a configured value.
func (f *runFlags) applyOverrides(r *config.Resolved) error {
	if f.set["profile"] {
		profile := analyze.Profile(f.profile).Resolve(r.Target.Kind)
		policy, err := analyze.PolicyFor(profile)
		if err != nil {
			return usagef("%w", err)
		}
		policy.MaxInFlightDropPct = r.Policy.MaxInFlightDropPct
		policy.MaxShutdownTime = r.Policy.MaxShutdownTime
		policy.MinScore = r.Policy.MinScore
		r.Policy = policy
		r.Termination.GracePeriod = policy.GracePeriod
	}

	if f.set["grace-period"] {
		r.Policy.GracePeriod = f.gracePeriod
		r.Termination.GracePeriod = f.gracePeriod
	}
	if f.set["accept-window"] {
		r.Policy.AcceptWindow = f.acceptWindow
	}
	if f.set["max-inflight-drop-pct"] {
		r.Policy.MaxInFlightDropPct = f.maxDropPct
	}
	if f.set["max-shutdown-time"] {
		d := f.maxShutdownTime
		r.Policy.MaxShutdownTime = &d
	}
	if f.set["min-score"] {
		s := f.minScore
		r.Policy.MinScore = &s
	}
	if f.set["trials"] {
		r.Trials = f.trials
	}
	if f.set["prestop-sleep"] {
		r.Termination.PreStopSleep = f.preStopSleep
	}
	if f.set["enforce-sigkill"] {
		r.Termination.EnforceSigkill = f.enforceSigkill
	}
	if f.set["readiness-url"] {
		r.Probe.ReadinessURL = f.readinessURL
	}
	if f.set["capture-target-logs"] {
		r.CaptureTargetLogs = f.captureLogs
	}

	for _, id := range f.ignore {
		if _, ok := analyze.Lookup(analyze.SignatureID(id)); !ok {
			return usagef("unknown signature %q in --ignore", id)
		}
		r.Policy = r.Policy.WithSeverity(analyze.SignatureID(id), schema.SeverityInfo)
	}
	for _, id := range f.failOn {
		if _, ok := analyze.Lookup(analyze.SignatureID(id)); !ok {
			return usagef("unknown signature %q in --fail-on", id)
		}
		r.Policy = r.Policy.WithSeverity(analyze.SignatureID(id), schema.SeverityError)
	}

	if !report.Format(f.format).Valid() {
		return usagef("unknown format %q; valid values are %v", f.format, report.Formats())
	}
	if f.stack != "" && !remediate.Stack(f.stack).Valid() {
		return usagef("unknown stack %q; valid values are %v", f.stack, remediate.Stacks())
	}
	if r.Trials < 1 {
		r.Trials = 1
	}
	return nil
}

type trialOutcome struct {
	analysis analyze.Result
	tl       timeline.Timeline
}

func executeTrials(ctx context.Context, flags *runFlags, resolved *config.Resolved, stderr io.Writer) (analyze.Result, error) {
	var (
		results []analyze.Result
		last    trialOutcome
	)

	for trial := 1; trial <= resolved.Trials; trial++ {
		if !flags.quiet && resolved.Trials > 1 {
			fmt.Fprintf(stderr, "trial %d of %d\n", trial, resolved.Trials)
		}

		outcome, err := executeOnce(ctx, flags, resolved, trial)
		if err != nil {
			return analyze.Result{}, err
		}
		results = append(results, outcome.analysis)
		last = outcome
	}

	aggregated, err := analyze.Aggregate(results)
	if err != nil {
		return analyze.Result{}, err
	}
	aggregated.Facts = last.analysis.Facts
	flags.lastTimeline = last.tl
	return aggregated, nil
}

func executeOnce(ctx context.Context, flags *runFlags, resolved *config.Resolved, trial int) (trialOutcome, error) {
	recorder := timeline.NewRecorder(timeline.Meta{
		ToolVersion: Version,
		StartedAt:   time.Now().UTC(),
		Target:      resolved.Target.Label,
		Profile:     string(resolved.Policy.Profile),
		Seed:        resolved.Traffic.Seed,
		Trial:       trial,
		Trials:      resolved.Trials,
	}, timeline.DefaultRecordLimit)

	origin := time.Now()
	now := func() time.Duration { return time.Since(origin) }

	tgt, err := buildTarget(resolved, recorder, now)
	if err != nil {
		return trialOutcome{}, &targetError{err}
	}
	defer func() { _ = tgt.Close() }()

	prober := probe.NewHTTP(probe.Options{
		Timeout:   resolved.Traffic.RequestTimeout,
		KeepAlive: resolved.Traffic.KeepAlive,
		Insecure:  resolved.Probe.Insecure,
		MaxConns:  resolved.Traffic.ConcurrencyCap,
		Now:       now,
		Sink:      recorder.Record,
	})
	defer prober.Close()

	requests := make([]probe.Request, 0, len(resolved.Probe.Requests))
	weights := make([]int, 0, len(resolved.Probe.Requests))
	for _, req := range resolved.Probe.Requests {
		requests = append(requests, probe.Request{
			Name: req.Name, Method: req.Method, URL: req.URL,
			Headers: req.Headers, Body: []byte(req.Body),
		})
		weights = append(weights, req.Weight)
	}

	generator := load.New(load.Options{
		Prober: prober, Clock: clock.System(), Recorder: recorder,
		Requests: requests, Weights: weights,
		ConcurrencyCap: resolved.Traffic.ConcurrencyCap,
	})

	options := run.Options{
		Target: tgt, Generator: generator, Recorder: recorder,
		Clock: clock.System(), Policy: resolved.Policy,
		ObserveInterval: flags.observeInterval,
		Warmup:          resolved.Traffic.Warmup,
		Steady:          resolved.Traffic.Steady,
		PreStopSleep:    resolved.Termination.PreStopSleep,
		Signal:          target.Signal(resolved.Termination.Signal),
		EnforceSigkill:  resolved.Termination.EnforceSigkill,
		TargetInFlight:  resolved.Traffic.EnsureInFlight,
		MaxRPS:          resolved.Traffic.MaxRPS,
		ConcurrencyCap:  resolved.Traffic.ConcurrencyCap,
	}
	if !resolved.Traffic.Calibrate {
		options.FixedRate = resolved.Traffic.RPS
	}

	if resolved.Probe.ReadinessURL != "" {
		options.Readiness = probe.NewReadiness(resolved.Probe.ReadinessURL, 2*time.Second, resolved.Probe.Insecure, now)
	}
	if addr, err := probe.AddrFromURL(resolved.Probe.Requests[0].URL); err == nil {
		options.Listener = probe.NewListener(addr, time.Second, now)
	}

	runner, err := run.New(options)
	if err != nil {
		return trialOutcome{}, usagef("%w", err)
	}

	outcome, err := runner.Run(ctx)
	if err != nil {
		return trialOutcome{}, &targetError{err}
	}

	analysis := analyze.Analyze(analyze.Input{
		Timeline:    outcome.Timeline,
		Policy:      resolved.Policy,
		ToolVersion: Version,
		Target: analyze.TargetInfo{
			Kind: resolved.Target.Kind, Label: resolved.Target.Label,
			PID:           tgt.Describe().PID,
			DetectedStack: string(remediate.DetectStack(remediate.Hints{Override: flags.stack, Argv: resolved.Target.Command})),
		},
		Probe: analyze.ProbeInfo{
			URL: resolved.Probe.Requests[0].URL, Method: resolved.Probe.Requests[0].Method,
			ReadinessURL: resolved.Probe.ReadinessURL,
		},
		Load: analyze.LoadInfo{
			Calibrated: outcome.Calibrated, RPS: outcome.Calibration.RPS,
			TargetInFlight: resolved.Traffic.EnsureInFlight,
			Achievable:     outcome.Calibration.Achievable,
			Warnings:       outcome.Calibration.Warnings,
		},
	})

	return trialOutcome{analysis: analysis, tl: outcome.Timeline}, nil
}

func buildTarget(resolved *config.Resolved, recorder *timeline.Recorder, now func() time.Duration) (target.Target, error) {
	ready := target.ReadyCheck{URL: resolved.Target.Ready.URL, Timeout: resolved.Target.Ready.Timeout}
	if ready.URL == "" && resolved.Probe.ReadinessURL != "" {
		ready.URL = resolved.Probe.ReadinessURL
	}
	if ready.URL == "" {
		if addr, err := probe.AddrFromURL(resolved.Probe.Requests[0].URL); err == nil {
			ready.Addr = addr
		}
	}

	var sink target.LogSink
	if resolved.CaptureTargetLogs {
		sink = func(stream, line string) {
			recorder.Record(timeline.LogAt(now(), stream, line))
		}
	}

	switch resolved.Target.Kind {
	case analyze.TargetCommand:
		return target.NewCommand(target.CommandOptions{
			Argv: resolved.Target.Command, Ready: ready, LogSink: sink,
			GracePeriod: resolved.Termination.GracePeriod,
		})
	case analyze.TargetProcess:
		return target.NewProcess(target.ProcessOptions{
			PID: resolved.Target.PID, Ready: ready, GracePeriod: resolved.Termination.GracePeriod,
		})
	default:
		return nil, fmt.Errorf("target kind %q is not supported yet", resolved.Target.Kind)
	}
}

func emit(flags *runFlags, result analyze.Result, stdout io.Writer) error {
	render := func(w io.Writer) error {
		switch report.Format(flags.format) {
		case report.FormatJSON:
			return report.JSON(w, result.Report)
		case report.FormatJUnit:
			return report.JUnit(w, result)
		case report.FormatMarkdown:
			return report.Markdown(w, result)
		case report.FormatNDJSON:
			return report.NDJSON(w, flags.lastTimeline)
		default:
			return report.Human(w, result, flags.lastTimeline, report.Options{
				Width: flags.width, NoColor: flags.noColor,
				NoTimeline: flags.noTimeline, Quiet: flags.quiet,
			})
		}
	}

	if err := render(stdout); err != nil {
		return err
	}

	if flags.output != "" {
		if err := writeFile(flags.output, render); err != nil {
			return err
		}
	}
	if flags.badge != "" {
		if err := writeFile(flags.badge, func(w io.Writer) error {
			return report.Badge(w, result.Report)
		}); err != nil {
			return err
		}
	}
	return nil
}

// writeFile writes a report with restrictive permissions, since reports carry
// internal hostnames and URLs.
func writeFile(path string, render func(io.Writer) error) error {
	f, err := os.OpenFile(filepath.Clean(path), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	if err := render(f); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
