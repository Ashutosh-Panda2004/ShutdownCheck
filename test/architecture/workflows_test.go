package architecture

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

var pinnedAction = regexp.MustCompile(`^[^\s]+@[0-9a-f]{40}$`)

func TestWorkflowYAMLAndActionRefs(t *testing.T) {
	paths, err := filepath.Glob("../../.github/workflows/*.yml")
	if err != nil {
		t.Fatalf("glob workflows: %v", err)
	}
	paths = append(paths, "../../action.yml")

	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path) // #nosec G304 -- paths are repository-owned
			if err != nil {
				t.Fatalf("read: %v", err)
			}

			var document any
			if err := yaml.Unmarshal(data, &document); err != nil {
				t.Fatalf("invalid YAML: %v", err)
			}

			for lineNumber, line := range strings.Split(string(data), "\n") {
				trimmed := strings.TrimSpace(line)
				if !strings.HasPrefix(trimmed, "uses:") && !strings.HasPrefix(trimmed, "- uses:") {
					continue
				}
				ref := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(trimmed, "- "), "uses:"))
				if before, _, found := strings.Cut(ref, " #"); found {
					ref = strings.TrimSpace(before)
				}
				if strings.HasPrefix(ref, "./") {
					continue
				}
				if !pinnedAction.MatchString(ref) {
					t.Errorf("line %d uses mutable Action ref %q; pin a 40-character commit SHA", lineNumber+1, ref)
				}
			}
		})
	}
}
