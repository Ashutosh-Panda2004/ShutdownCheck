package conformance

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/shutdowncheck/shutdowncheck/internal/analyze"
	"github.com/shutdowncheck/shutdowncheck/pkg/schema"
)

// This file holds the contract every language implementation is held to. It
// carries no build tag, so the coverage guarantee below is checked on every
// platform even though the servers themselves only run where signals exist.

// scenario is one shutdown defect and what ShutdownCheck must conclude about it.
type scenario struct {
	mode    string
	profile string
	grace   string
	accept  string

	// verdict is what the run must reach. An empty value means "anything except
	// pass", used where the exact severity depends on profile defaults.
	verdict schema.Verdict

	mustFire    []analyze.SignatureID
	mustNotFire []analyze.SignatureID
}

var scenarios = []scenario{
	{
		mode: "correct", profile: "standalone", grace: "4s",
		verdict: schema.VerdictPass,
		mustNotFire: []analyze.SignatureID{
			analyze.SC001, analyze.SC002, analyze.SC003,
			analyze.SC004, analyze.SC011, analyze.SC012,
		},
	},
	{
		mode: "ignore-signal", profile: "standalone", grace: "2s",
		verdict:  schema.VerdictFail,
		mustFire: []analyze.SignatureID{analyze.SC002},
	},
	{
		// Correct by the naive reading of "shut down gracefully", and a source
		// of 502s behind any load balancer.
		mode: "instant-close", profile: "kubernetes", grace: "6s", accept: "1s",
		verdict:  schema.VerdictFail,
		mustFire: []analyze.SignatureID{analyze.SC006, analyze.SC007},
	},
	{
		// The same evidence, judged where closing immediately is the requirement.
		mode: "instant-close", profile: "strict", grace: "4s",
		mustNotFire: []analyze.SignatureID{analyze.SC006},
	},
	{
		mode: "no-readiness-flip", profile: "kubernetes", grace: "6s", accept: "1s",
		verdict:  schema.VerdictFail,
		mustFire: []analyze.SignatureID{analyze.SC007},
	},
	{
		mode: "abrupt-reset", profile: "standalone", grace: "4s",
		verdict:  schema.VerdictFail,
		mustFire: []analyze.SignatureID{analyze.SC003},
	},
	{
		mode: "slow-drain", profile: "standalone", grace: "2s",
		verdict:  schema.VerdictFail,
		mustFire: []analyze.SignatureID{analyze.SC002},
	},
	{
		mode: "early-exit", profile: "standalone", grace: "4s",
		verdict: schema.VerdictFail,
	},
	{
		mode: "listener-never-closes", profile: "lame-duck", grace: "4s", accept: "800ms",
		verdict:  schema.VerdictFail,
		mustFire: []analyze.SignatureID{analyze.SC005},
	},
	{
		mode: "slow-readiness", profile: "kubernetes", grace: "6s", accept: "1s",
		verdict:  schema.VerdictFail,
		mustFire: []analyze.SignatureID{analyze.SC008},
	},
	{
		mode: "readiness-flap", profile: "kubernetes", grace: "6s", accept: "1s",
		mustFire: []analyze.SignatureID{analyze.SC016},
	},
	{
		mode: "nonzero-exit", profile: "standalone", grace: "4s",
		mustFire: []analyze.SignatureID{analyze.SC013},
	},
	{
		mode: "accept-no-response", profile: "standalone", grace: "3s",
		verdict:  schema.VerdictFail,
		mustFire: []analyze.SignatureID{analyze.SC015},
	},
	{
		mode: "orphan-child", profile: "standalone", grace: "4s",
		verdict:  schema.VerdictFail,
		mustFire: []analyze.SignatureID{analyze.SC012},
	},
}

// language describes how to obtain a runnable server for one stack.
type language struct {
	name string
	// prepare returns the argv prefix to launch the server, or a reason to skip
	// when the toolchain is not installed.
	prepare func(t *testing.T) (argv []string, skip string)
	// modes lists the scenarios this implementation covers. Not every stack
	// expresses every defect naturally, and asserting one a fixture does not
	// actually exhibit would be testing fiction.
	modes map[string]bool
}

func allModes(except ...string) map[string]bool {
	skip := map[string]bool{}
	for _, mode := range except {
		skip[mode] = true
	}

	out := map[string]bool{}
	for _, s := range scenarios {
		if !skip[s.mode] {
			out[s.mode] = true
		}
	}
	return out
}

func languages() []language {
	return []language{
		{name: "go", prepare: buildGoServer, modes: allModes()},
		{
			name:    "node",
			prepare: interpretedServer("node", filepath.Join("node", "server.js")),
			modes:   allModes("orphan-child"),
		},
		{
			name:    "python",
			prepare: interpretedServer("python3", filepath.Join("python", "server.py")),
			modes:   allModes("orphan-child"),
		},
	}
}

func buildGoServer(t *testing.T) ([]string, string) {
	t.Helper()

	binary := filepath.Join(t.TempDir(), "conformance-go")
	cmd := exec.Command("go", "build", "-o", binary, "./go")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, "building the Go server failed: " + err.Error()
	}
	return []string{binary}, ""
}

func interpretedServer(interpreter, script string) func(*testing.T) ([]string, string) {
	return func(t *testing.T) ([]string, string) {
		t.Helper()

		path, err := exec.LookPath(interpreter)
		if err != nil {
			return nil, interpreter + " is not installed"
		}
		if _, err := os.Stat(script); err != nil {
			return nil, script + " is missing"
		}
		return []string{path, script}, ""
	}
}

// singleLanguageSignatures are defects of process topology rather than of any
// framework: they are about file-descriptor inheritance and process groups, and
// behave identically whatever the service is written in.
//
// Reimplementing them per language would test the fixture's plumbing rather
// than the tool, so one faithful implementation is enough. Anything not listed
// here needs two, because it depends on framework behaviour that genuinely
// differs between stacks.
var singleLanguageSignatures = map[analyze.SignatureID]string{
	analyze.SC012: "an orphaned listener is inherited through a file descriptor, which is stack-independent",
}

// Every signature the suite claims to cover must be exercised in at least two
// independent stacks, unless it is one of the topology defects above. Without
// this the cross-stack claim decays silently as scenarios are edited, and
// "works regardless of your stack" quietly becomes "works in Go".
func TestSignatureCoverageAcrossLanguages(t *testing.T) {
	perSignature := map[analyze.SignatureID]map[string]bool{}

	for _, lang := range languages() {
		for _, s := range scenarios {
			if !lang.modes[s.mode] {
				continue
			}
			for _, id := range s.mustFire {
				if perSignature[id] == nil {
					perSignature[id] = map[string]bool{}
				}
				perSignature[id][lang.name] = true
			}
		}
	}

	if len(perSignature) == 0 {
		t.Fatal("no signatures are asserted by any scenario")
	}

	for _, info := range analyze.Catalog() {
		langs := perSignature[info.ID]
		switch {
		case len(langs) == 0:
			// Covered by synthetic timeline fixtures in internal/analyze.
			continue
		case len(langs) >= 2:
			continue
		case singleLanguageSignatures[info.ID] != "":
			continue
		default:
			t.Errorf("%s is only exercised by %v; the cross-stack claim needs at least two",
				info.ID, keys(langs))
		}
	}
}

// An exemption that is no longer needed should be removed rather than left to
// quietly excuse a gap that has since been filled.
func TestSingleLanguageExemptionsAreStillNecessary(t *testing.T) {
	for id, reason := range singleLanguageSignatures {
		if _, ok := analyze.Lookup(id); !ok {
			t.Errorf("%s is exempted but is not a real signature", id)
		}
		if reason == "" {
			t.Errorf("%s is exempted without a reason", id)
		}

		covering := 0
		for _, lang := range languages() {
			for _, s := range scenarios {
				if !lang.modes[s.mode] {
					continue
				}
				for _, asserted := range s.mustFire {
					if asserted == id {
						covering++
					}
				}
			}
		}
		if covering >= 2 {
			t.Errorf("%s now has %d language implementations; drop the exemption", id, covering)
		}
	}
}

// A scenario asserting a signature that does not exist would silently never
// fire, and the suite would look greener than it is.
func TestScenariosReferenceRealSignatures(t *testing.T) {
	for _, s := range scenarios {
		for _, id := range append(append([]analyze.SignatureID{}, s.mustFire...), s.mustNotFire...) {
			if _, ok := analyze.Lookup(id); !ok {
				t.Errorf("scenario %q references unknown signature %s", s.mode, id)
			}
		}
	}
}

func TestEveryLanguageCoversTheCorrectMode(t *testing.T) {
	for _, lang := range languages() {
		if !lang.modes["correct"] {
			t.Errorf("%s does not implement the correct mode; without it we never prove a clean pass", lang.name)
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
