package cli

import (
	"flag"
	"io"
	"strings"
	"testing"
)

func runFlagSet(t *testing.T) *flag.FlagSet {
	t.Helper()

	flags, err := parseRunFlags(nil, io.Discard)
	if err != nil {
		t.Fatalf("parseRunFlags: %v", err)
	}
	return flags.fs
}

// Every registered flag has to appear in the help output.
//
// This used to be untrue in a particularly unhelpful way: the run usage ended
// with "run --help to see every flag", and --help printed that same text. The
// promise was circular, and any flag added later was invisible unless someone
// happened to read the source.
func TestEveryRunFlagAppearsInHelp(t *testing.T) {
	_, _, help := execute(t, "run", "--help")
	if help == "" {
		t.Fatal("run --help produced nothing")
	}

	var undocumented []string
	runFlagSet(t).VisitAll(func(f *flag.Flag) {
		if !strings.Contains(help, "-"+f.Name) {
			undocumented = append(undocumented, "--"+f.Name)
		}
	})

	if len(undocumented) > 0 {
		t.Errorf("these flags exist but are not in `run --help`: %s\n"+
			"a flag nobody can discover may as well not be there",
			strings.Join(undocumented, ", "))
	}
}

// A flag listed with no explanation is barely more discoverable than a missing
// one.
func TestEveryRunFlagExplainsItself(t *testing.T) {
	runFlagSet(t).VisitAll(func(f *flag.Flag) {
		if strings.TrimSpace(f.Usage) == "" {
			t.Errorf("--%s has no usage text", f.Name)
		}
	})
}

// The curated section is what makes the help readable; losing it to a bare
// alphabetical dump would be a regression even though every flag is listed.
func TestRunHelpKeepsItsGuidance(t *testing.T) {
	_, _, help := execute(t, "run", "--help")

	for _, want := range []string{
		"Target (choose exactly one)",
		`Profiles decide what "correct" means`,
		"All flags:",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("run help is missing %q", want)
		}
	}
}

func TestTopLevelHelpListsEveryCommand(t *testing.T) {
	_, stdout, _ := execute(t, "help")

	for _, command := range []string{"run", "analyze", "demo", "explain", "validate", "version"} {
		if !strings.Contains(stdout, command) {
			t.Errorf("top-level help does not mention %q", command)
		}
	}
}
