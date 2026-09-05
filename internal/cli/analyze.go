package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/shutdowncheck/shutdowncheck/internal/analyze"
	"github.com/shutdowncheck/shutdowncheck/internal/redact"
	"github.com/shutdowncheck/shutdowncheck/internal/report"
	"github.com/shutdowncheck/shutdowncheck/internal/timeline"
	"github.com/shutdowncheck/shutdowncheck/pkg/schema"
)

// analyzeCommand re-judges a recorded run without repeating it.
//
// This is possible because analysis is a pure function of the evidence, so a
// timeline captured once can be re-read under a different shutdown profile.
// It is also how a maintainer investigates a misdiagnosis report: the reporter
// attaches run.ndjson, and the exact verdict can be reproduced offline.
func analyzeCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("analyze", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	profile := fs.String("profile", "", "re-judge under a different shutdown profile")
	format := fs.String("format", string(report.FormatHuman), "output format")
	width := fs.Int("width", 0, "terminal width for the report")
	noColor := fs.Bool("no-color", false, "disable coloured output")
	noTimeline := fs.Bool("no-timeline", false, "omit the timeline visualisation")
	fs.Usage = func() { printAnalyzeUsage(stderr) }

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printAnalyzeUsage(stderr)
			return schema.ExitPass
		}
		return exitFor(usagef("%w", err), stderr)
	}

	// flag stops at the first positional argument, so parse what follows the
	// filename as well. That way `analyze run.ndjson --profile k8s` works as
	// naturally as putting the flags first.
	rest := fs.Args()
	if len(rest) == 0 {
		printAnalyzeUsage(stderr)
		return schema.ExitUsage
	}
	path := rest[0]

	if err := fs.Parse(rest[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printAnalyzeUsage(stderr)
			return schema.ExitPass
		}
		return exitFor(usagef("%w", err), stderr)
	}
	if fs.NArg() != 0 {
		return exitFor(usagef("analyze takes exactly one recording, got %d", len(rest)), stderr)
	}

	tl, err := readTimeline(path)
	if err != nil {
		return exitFor(usagef("%w", err), stderr)
	}

	input, trials, err := inputForReanalysis(tl, *profile)
	if err != nil {
		return exitFor(err, stderr)
	}
	input.ToolVersion = Version
	result := analyze.Analyze(input)
	if *profile == "" && tl.Meta.Analysis != nil {
		result.Report.Run.Trials = schema.Trials{
			Total: trials.Total, Failed: trials.Failed, Consistent: trials.Consistent,
		}
	}

	if !report.Format(*format).Valid() {
		return exitFor(usagef("unknown format %q; valid values are %v", *format, report.Formats()), stderr)
	}

	var renderErr error
	switch report.Format(*format) {
	case report.FormatJSON:
		renderErr = report.JSON(stdout, result.Report)
	case report.FormatJUnit:
		renderErr = report.JUnit(stdout, result)
	case report.FormatMarkdown:
		renderErr = report.Markdown(stdout, result)
	case report.FormatNDJSON:
		renderErr = report.NDJSON(stdout, tl)
	default:
		renderErr = report.Human(stdout, result, tl, report.Options{
			Width: *width, NoColor: *noColor, NoTimeline: *noTimeline,
		})
	}
	if renderErr != nil {
		return exitFor(renderErr, stderr)
	}

	return result.Report.Verdict.ExitCode()
}

func readTimeline(path string) (timeline.Timeline, error) {
	f, err := os.Open(filepath.Clean(path)) // #nosec G304 -- operator-supplied path
	if err != nil {
		return timeline.Timeline{}, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	tl, err := timeline.ReadNDJSON(f)
	if err != nil {
		return timeline.Timeline{}, fmt.Errorf("read %s: %w", path, err)
	}
	return tl, nil
}

// policyForReanalysis prefers the profile the caller asked for, falling back to
// whichever one produced the recording.
func inputForReanalysis(tl timeline.Timeline, override string) (analyze.Input, timeline.AnalysisTrials, error) {
	if override == "" && tl.Meta.Analysis != nil {
		return decodeAnalysisContext(tl)
	}

	name := override
	if name == "" {
		name = tl.Meta.Profile
	}
	if name == "" {
		name = string(analyze.ProfileStandalone)
	}

	kind := analyze.TargetProcess
	input := analyze.Input{Timeline: tl, Target: analyze.TargetInfo{Label: redact.Text(tl.Meta.Target)}}
	if tl.Meta.Analysis != nil {
		stored, _, err := decodeAnalysisContext(tl)
		if err != nil {
			return analyze.Input{}, timeline.AnalysisTrials{}, usagef("%w", err)
		}
		input.Target = stored.Target
		input.Probe = stored.Probe
		input.Load = stored.Load
		kind = stored.Target.Kind
	}

	profile := analyze.Profile(name).Resolve(kind)
	policy, err := analyze.PolicyFor(profile)
	if err != nil {
		return analyze.Input{}, timeline.AnalysisTrials{}, usagef("%w", err)
	}
	input.Policy = policy
	return input, timeline.AnalysisTrials{Total: 1, Consistent: true}, nil
}

func printAnalyzeUsage(w io.Writer) {
	writeBestEffort(w, `Usage: shutdowncheck analyze <run.ndjson> [flags]

Re-judges a recorded run without running it again.

Flags:
  --profile <name>   re-judge under a different shutdown profile
  --format <name>    human, json, junit, markdown, ndjson
  --no-color         disable coloured output
  --no-timeline      omit the timeline visualisation

Capture a run with:
  shutdowncheck run ... --format ndjson --output run.ndjson

The same evidence can then be judged under a different deployment model, which
is useful when deciding whether behaviour that is correct standalone would be
correct behind a load balancer.

`)
}
