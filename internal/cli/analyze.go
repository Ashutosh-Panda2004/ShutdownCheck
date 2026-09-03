package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/shutdowncheck/shutdowncheck/internal/analyze"
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
		if err == flag.ErrHelp {
			printAnalyzeUsage(stderr)
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
		if err == flag.ErrHelp {
			printAnalyzeUsage(stderr)
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

	policy, err := policyForReanalysis(tl, *profile)
	if err != nil {
		return exitFor(err, stderr)
	}

	result := analyze.Analyze(analyze.Input{
		Timeline:    tl,
		Policy:      policy,
		ToolVersion: Version,
		Target:      analyze.TargetInfo{Label: tl.Meta.Target},
	})

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
func policyForReanalysis(tl timeline.Timeline, override string) (analyze.Policy, error) {
	name := override
	if name == "" {
		name = tl.Meta.Profile
	}
	if name == "" {
		name = string(analyze.ProfileStandalone)
	}

	profile := analyze.Profile(name).Resolve(analyze.TargetProcess)
	policy, err := analyze.PolicyFor(profile)
	if err != nil {
		return analyze.Policy{}, usagef("%w", err)
	}
	return policy, nil
}

func printAnalyzeUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: shutdowncheck analyze <run.ndjson> [flags]

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
