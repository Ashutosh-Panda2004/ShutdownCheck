package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/config"
)

func TestReadRequestBodyRejectsOversizedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "too-large.bin")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", MaxRequestBodyBytes+1)), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := readRequestBody(path); err == nil {
		t.Fatal("oversized request body was accepted")
	}
}

func TestConfiguredBodyFileIsLoadedRelativeToConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "body.json"), []byte(`{"ok":true}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	resolved := &config.Resolved{Probe: config.Probe{Requests: []config.Request{{
		Name: "create", BodyFile: "body.json",
	}}}}
	if err := materializeRequestBodies(resolved, dir); err != nil {
		t.Fatalf("materializeRequestBodies: %v", err)
	}
	if got := resolved.Probe.Requests[0].Body; got != `{"ok":true}` {
		t.Errorf("Body = %q", got)
	}
	if resolved.Probe.Requests[0].BodyFile != "" {
		t.Error("BodyFile should be cleared after it is loaded")
	}
}

func TestConfiguredBodyFilesHaveAggregateLimit(t *testing.T) {
	dir := t.TempDir()
	body := strings.Repeat("x", MaxRequestBodyBytes)
	if err := os.WriteFile(filepath.Join(dir, "body.bin"), []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	requests := make([]config.Request, 4)
	for i := range requests {
		requests[i] = config.Request{Name: "request", BodyFile: "body.bin"}
	}
	resolved := &config.Resolved{Probe: config.Probe{Requests: requests}}

	if err := materializeRequestBodies(resolved, dir); err == nil {
		t.Fatal("aggregate request body limit was not enforced")
	}
}

func TestExplicitBodyOverridesMissingConfiguredBodyFile(t *testing.T) {
	path := writeConfig(t, `
version: 1
scenarios:
  api:
    target:
      command: ["./api"]
    probe:
      requests:
        - url: http://localhost:8080/work
          body_file: missing.json
`)

	flags := resolveRunFlags(t, "--config", path, "--body", `{"from":"cli"}`)
	resolved, err := flags.resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	request := resolved.Probe.Requests[0]
	if request.Body != `{"from":"cli"}` || request.BodyFile != "" {
		t.Errorf("request = %+v", request)
	}
}
