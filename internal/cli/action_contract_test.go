package cli

import (
	"encoding/json"
	"testing"

	"github.com/Ashutosh-Panda2004/ShutdownCheck/pkg/schema"
)

// The GitHub Action reads the verdict and score out of the JSON
// report with jq, using these exact paths. Nothing in the compiler connects the
// two, so renaming a field would break every user's pipeline silently and only
// at run time. This is the test that makes that a build failure instead.
func TestActionJSONPathsAreStable(t *testing.T) {
	path := instantCloseNDJSON(t)

	code, stdout, stderr := execute(t, "analyze", path, "--format", "json", "--no-color")
	if code != schema.ExitPass {
		t.Fatalf("exit = %d\nstderr: %s", code, stderr)
	}

	var report map[string]any
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("analyze --format json did not produce valid JSON: %v", err)
	}

	// jq: .verdict
	verdict, ok := report["verdict"].(string)
	if !ok || verdict == "" {
		t.Errorf(`.verdict is missing or not a string; action.yml reads it with jq -r '.verdict'`)
	}

	// jq: .score.value
	score, ok := report["score"].(map[string]any)
	if !ok {
		t.Fatalf(`.score is missing or not an object; action.yml reads jq -r '.score.value'`)
	}
	if _, ok := score["value"].(float64); !ok {
		t.Errorf(`.score.value is missing or not a number; action/action.yml depends on it`)
	}
}
