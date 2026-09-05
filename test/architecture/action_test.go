package architecture

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func actionManifest(t *testing.T) ([]byte, map[string]any) {
	t.Helper()

	data, err := os.ReadFile("../../action.yml")
	if err != nil {
		t.Fatalf("the Action must be discoverable at the repository root: %v", err)
	}

	var manifest map[string]any
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("action.yml is not valid YAML: %v", err)
	}
	return data, manifest
}

func TestActionManifestIsRootDiscoverable(t *testing.T) {
	_, manifest := actionManifest(t)

	if _, err := os.Stat("../../action/action.yml"); !os.IsNotExist(err) {
		t.Fatalf("action/action.yml still exists; examples using owner/repo@ref would not load it")
	}

	runs, ok := manifest["runs"].(map[string]any)
	if !ok {
		t.Fatal("action.yml has no runs mapping")
	}
	if runs["using"] != "composite" {
		t.Errorf("runs.using = %v, want composite", runs["using"])
	}
	if _, ok := manifest["inputs"].(map[string]any); !ok {
		t.Error("action.yml has no inputs mapping")
	}
	if _, ok := manifest["outputs"].(map[string]any); !ok {
		t.Error("action.yml has no outputs mapping")
	}
}

func TestActionUsesPinnedLocalInstaller(t *testing.T) {
	data, _ := actionManifest(t)
	text := string(data)

	if !strings.Contains(text, `sh "$GITHUB_ACTION_PATH/install.sh"`) {
		t.Error("the Action does not use the installer from its pinned revision")
	}
	if strings.Contains(text, "raw.githubusercontent.com/shutdowncheck/shutdowncheck/main/install.sh") {
		t.Error("the Action downloads its installer from a moving branch")
	}
	if strings.Contains(text, "default: latest") || !strings.Contains(text, `SHUTDOWNCHECK_VERSION" = "latest"`) {
		if !strings.Contains(text, `IN_VERSION" = "latest"`) {
			t.Error("the Action can download a moving binary version instead of requiring an exact release")
		}
	}
}

func TestActionCannotSuppressOperationalFailures(t *testing.T) {
	data, _ := actionManifest(t)
	text := string(data)

	if !strings.Contains(text, `if [ "${CODE:-5}" -ge 2 ]; then`) {
		t.Error("fail-on-error can suppress inconclusive or operational failures")
	}
	if !strings.Contains(text, `true|false)`) {
		t.Error("invalid fail-on-error values can silently disable gating")
	}
	if !strings.Contains(text, "- name: Validate inputs") ||
		!strings.Contains(text, "validate_array command") ||
		!strings.Contains(text, "validate_bool upload-artifact") {
		t.Error("Action inputs are not validated before target execution")
	}
	if !strings.Contains(text, `steps.check.outputs.report-written == 'true'`) {
		t.Error("artifacts are not gated on a report actually having been produced")
	}
}

func TestActionPublishesTheOriginalRunReport(t *testing.T) {
	data, _ := actionManifest(t)
	text := string(data)

	if !strings.Contains(text, `args=(run --format json --output "$report" --no-color)`) {
		t.Error("the Action does not request its JSON artifact from the original run")
	}
	if strings.Contains(text, `shutdowncheck analyze`) {
		t.Error("the Action re-analyzes evidence and can lose custom gates or policy overrides")
	}
	if !strings.Contains(text, `rm -f "$report" "$summary"`) {
		t.Error("the Action can reuse a stale report from an earlier invocation in the same job")
	}
}
