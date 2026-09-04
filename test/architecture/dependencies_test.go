package architecture

import (
	"os"
	"strings"
	"testing"
)

// The dependency budget is a security property, not an aesthetic one.
//
// This tool is meant to run inside CI pipelines next to production credentials,
// and every direct dependency is code that a reviewer has to trust and that a
// supply-chain attack can enter through. CONTRIBUTING.md sets the limit at ten;
// the point of the test is that adding the eleventh has to be a deliberate,
// visible decision rather than something that happens in passing.
const maxDirectDependencies = 10

func TestDependencyBudget(t *testing.T) {
	direct := directDependencies(t)

	if len(direct) > maxDirectDependencies {
		t.Errorf("%d direct dependencies, budget is %d: %s",
			len(direct), maxDirectDependencies, strings.Join(direct, ", "))
	}
	t.Logf("%d direct dependencies: %s", len(direct), strings.Join(direct, ", "))
}

// directDependencies returns the modules in go.mod that are not marked
// indirect. Parsed by hand rather than with golang.org/x/mod, since pulling in
// a dependency to count dependencies would be its own answer to the question.
func directDependencies(t *testing.T) []string {
	t.Helper()

	data, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}

	var (
		direct   []string
		inBlock  bool
		lineIter = strings.Split(string(data), "\n")
	)

	for _, raw := range lineIter {
		line := strings.TrimSpace(raw)

		switch {
		case strings.HasPrefix(line, "require ("):
			inBlock = true
			continue
		case inBlock && line == ")":
			inBlock = false
			continue
		case strings.HasPrefix(line, "require "):
			// Single-line form: require example.com/x v1.2.3
			line = strings.TrimPrefix(line, "require ")
		case !inBlock:
			continue
		}

		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		if strings.Contains(line, "// indirect") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.Contains(fields[0], ".") {
			direct = append(direct, fields[0])
		}
	}
	return direct
}
