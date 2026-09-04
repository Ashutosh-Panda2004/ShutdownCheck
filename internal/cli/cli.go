package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/shutdowncheck/shutdowncheck/internal/analyze"
	"github.com/shutdowncheck/shutdowncheck/internal/config"
	"github.com/shutdowncheck/shutdowncheck/internal/remediate"
	"github.com/shutdowncheck/shutdowncheck/pkg/schema"
)

// Build information, set from main via ldflags.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// usageError marks a problem with how the tool was invoked, as opposed to a
// problem with the target. They exit differently so a pipeline can tell "you
// configured me wrong" from "your service has a bug".
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

func usagef(format string, args ...any) error {
	return &usageError{fmt.Errorf(format, args...)}
}

// targetError marks a failure to start, reach or signal the target.
type targetError struct{ err error }

func (e *targetError) Error() string { return e.err.Error() }
func (e *targetError) Unwrap() error { return e.err }

// Main runs the CLI and returns the process exit code.
//
// It takes its streams as parameters so the whole command surface, including
// exit codes, is testable without building a binary or capturing os.Stdout.
func Main(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return schema.ExitUsage
	}

	command, rest := args[0], args[1:]

	var err error
	switch command {
	case "run":
		return runCommand(rest, stdout, stderr)
	case "analyze", "analyse":
		return analyzeCommand(rest, stdout, stderr)
	case "demo":
		return demoCommand(rest, stdout, stderr)
	case demoServerCommand:
		return demoServer(rest, stderr)
	case "explain":
		err = explainCommand(rest, stdout)
	case "validate":
		err = validateCommand(rest, stdout)
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "shutdowncheck %s (commit %s, built %s)\n", Version, Commit, Date)
		return schema.ExitPass
	case "help", "--help", "-h":
		printUsage(stdout)
		return schema.ExitPass
	default:
		fmt.Fprintf(stderr, "shutdowncheck: unknown command %q\n\n", command)
		printUsage(stderr)
		return schema.ExitUsage
	}

	return exitFor(err, stderr)
}

// exitFor maps an error to an exit code, per spec section 9.3.
func exitFor(err error, stderr io.Writer) int {
	if err == nil {
		return schema.ExitPass
	}

	var usage *usageError
	var target *targetError

	switch {
	case errors.As(err, &usage):
		fmt.Fprintf(stderr, "shutdowncheck: %v\n", err)
		return schema.ExitUsage
	case errors.As(err, &target):
		fmt.Fprintf(stderr, "shutdowncheck: %v\n", err)
		return schema.ExitTarget
	default:
		fmt.Fprintf(stderr, "shutdowncheck: %v\n", err)
		return schema.ExitInternal
	}
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `shutdowncheck verifies that a service drains correctly when it is terminated.

Usage:
  shutdowncheck run [flags] [-- <command> [args...]]
  shutdowncheck analyze <run.ndjson> [flags]
  shutdowncheck demo [flags]
  shutdowncheck explain [SIGNATURE]
  shutdowncheck validate [--config <path>]
  shutdowncheck version

Commands:
  run        Terminate a target under load and report what broke
  analyze    Re-judge a recorded run, optionally under a different profile
  demo       Run a real check against a deliberately broken service
  explain    Describe a failure signature and how to fix it
  validate   Check a shutdowncheck.yaml without running anything
  version    Print build information

New here? Run "shutdowncheck demo" to see what a report looks like.
Run "shutdowncheck run --help" for the full list of run flags.

Exit codes:
  0  pass          shutdown behaviour was verified as correct
  1  fail          one or more defects were detected
  2  inconclusive  the run did not prove anything; this is not a pass
  3  usage         the tool was invoked incorrectly
  4  target        the target could not be started, reached or signalled
  5  internal      a bug in shutdowncheck
`)
}

func explainCommand(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("explain", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return usagef("%w", err)
	}

	if fs.NArg() == 0 {
		listSignatures(stdout)
		return nil
	}

	id := analyze.SignatureID(strings.ToUpper(fs.Arg(0)))
	page, err := remediate.Explain(id)
	if err != nil {
		return usagef("%w", err)
	}

	fmt.Fprint(stdout, page)
	return nil
}

func listSignatures(w io.Writer) {
	fmt.Fprintf(w, "Failure signatures. Run `shutdowncheck explain <ID>` for detail.\n\n")

	for _, info := range analyze.Catalog() {
		stage := string(info.Stage)
		if stage == "" {
			stage = "--"
		}
		fmt.Fprintf(w, "  %-6s %-2s  %-28s %s\n", info.ID, stage, info.Name, info.Summary)
	}

	fmt.Fprint(w, "\nStages refer to the seven stages of correct termination; see the project documentation.\n")
}

func validateCommand(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	path := fs.String("config", config.DefaultConfigPath, "path to the configuration file")
	if err := fs.Parse(args); err != nil {
		return usagef("%w", err)
	}

	file, err := config.Load(*path)
	if err != nil {
		return usagef("%w", err)
	}

	names := file.ScenarioNames()
	fmt.Fprintf(stdout, "%s is valid: %d scenario(s)\n", *path, len(names))

	for _, name := range names {
		resolved, err := file.Resolve(name)
		if err != nil {
			return usagef("scenario %q: %w", name, err)
		}
		fmt.Fprintf(stdout, "  %-20s target=%s profile=%s trials=%d\n",
			name, resolved.Target.Label, resolved.Policy.Profile, resolved.Trials)
	}
	return nil
}
