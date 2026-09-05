package config

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/shutdowncheck/shutdowncheck/internal/analyze"
	"github.com/shutdowncheck/shutdowncheck/pkg/schema"
)

// specExample is the configuration from spec section 12. Keeping it as a test
// input means documentation and code cannot drift apart silently.
const specExample = `
version: 1

defaults:
  profile: kubernetes
  grace_period: 30s
  ensure_in_flight: 20
  trials: 3
  capture_target_logs: true

scenarios:
  api:
    target:
      command: ["./bin/api", "--port", "8080"]
      ready:
        url: http://localhost:8080/readyz
        timeout: 30s
    probe:
      readiness_url: http://localhost:8080/readyz
      requests:
        - weight: 80
          url: http://localhost:8080/api/orders
          method: GET
        - weight: 20
          url: http://localhost:8080/api/orders
          method: POST
          headers:
            Content-Type: application/json
          body_file: ./testdata/order.json
    load:
      ensure_in_flight: 25
      steady: 8s
      request_timeout: 10s
    termination:
      prestop_sleep: 5s
      accept_window: 5s
    gate:
      max_inflight_drop_pct: 0
      max_shutdown_time: 15s
      min_score: 90
      ignore: [SC009]
`

func TestParseSpecExample(t *testing.T) {
	file, err := Parse([]byte(specExample))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if file.Version != 1 {
		t.Errorf("version = %d, want 1", file.Version)
	}
	if got := file.ScenarioNames(); len(got) != 1 || got[0] != "api" {
		t.Fatalf("scenarios = %v, want [api]", got)
	}

	resolved, err := file.Resolve("api")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if resolved.Target.Kind != analyze.TargetCommand {
		t.Errorf("target kind = %q, want command", resolved.Target.Kind)
	}
	if resolved.Policy.Profile != analyze.ProfileKubernetes {
		t.Errorf("profile = %q, want kubernetes", resolved.Policy.Profile)
	}
	if resolved.Trials != 3 {
		t.Errorf("trials = %d, want 3", resolved.Trials)
	}
	if !resolved.CaptureTargetLogs {
		t.Error("capture_target_logs should be true")
	}
	if resolved.Traffic.EnsureInFlight != 25 {
		t.Errorf("ensure_in_flight = %d, want 25 (scenario overrides defaults)", resolved.Traffic.EnsureInFlight)
	}
	if !resolved.Traffic.Calibrate {
		t.Error("calibration should be on when rps is not set")
	}
	if resolved.Policy.GracePeriod != 30*time.Second {
		t.Errorf("grace period = %v, want 30s", resolved.Policy.GracePeriod)
	}
	if resolved.Termination.PreStopSleep != 5*time.Second {
		t.Errorf("prestop sleep = %v, want 5s", resolved.Termination.PreStopSleep)
	}
	if !resolved.Termination.EnforceSigkill {
		t.Error("enforce_sigkill should default to true")
	}
	if got := resolved.Policy.SeverityOf(analyze.SC009); got != schema.SeverityInfo {
		t.Errorf("SC009 severity = %q, want info (it is in the ignore list)", got)
	}
	if resolved.Policy.MinScore == nil || *resolved.Policy.MinScore != 90 {
		t.Errorf("min_score = %v, want 90", resolved.Policy.MinScore)
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	input := `
version: 1
scenarios:
  api:
    target:
      command: ["./api"]
    probe:
      requests:
        - url: http://localhost:8080/
    gate:
      max_inflight_drop_pctt: 0
`
	if _, err := Parse([]byte(input)); err == nil {
		t.Fatal("a misspelled key must be rejected, not silently ignored")
	}
}

func TestParseRejectsTrailingYAMLDocument(t *testing.T) {
	input := `
version: 1
scenarios:
	api:
		target:
			command: ["./api"]
		probe:
			requests:
				- url: http://localhost:8080/
---
version: 1
scenarios:
	ignored:
		target:
			command: ["./other"]
`
	if _, err := Parse([]byte(input)); err == nil {
		t.Fatal("a second YAML document was silently ignored")
	}
}

func TestParseRejectsWrongVersion(t *testing.T) {
	for _, input := range []string{
		"scenarios:\n  a:\n    target:\n      pid: 5\n",
		"version: 99\nscenarios:\n  a:\n    target:\n      pid: 5\n",
	} {
		if _, err := Parse([]byte(input)); err == nil {
			t.Errorf("expected a version error for:\n%s", input)
		}
	}
}

func TestParseEmptyDocument(t *testing.T) {
	_, err := Parse([]byte(""))
	if !errors.Is(err, ErrNoScenarios) {
		t.Fatalf("Parse(empty) = %v, want ErrNoScenarios", err)
	}
}

func TestDurationParsing(t *testing.T) {
	valid := "version: 1\ndefaults:\n  grace_period: 1m30s\nscenarios:\n  a:\n    target:\n      pid: 5\n    probe:\n      requests:\n        - url: http://x/\n"
	file, err := Parse([]byte(valid))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := file.Defaults.GracePeriod.Duration(); got != 90*time.Second {
		t.Errorf("grace_period = %v, want 90s", got)
	}

	for name, raw := range map[string]string{
		"not a duration": "banana",
		"bare number":    "30",
		"negative":       "-5s",
	} {
		t.Run(name, func(t *testing.T) {
			input := "version: 1\ndefaults:\n  grace_period: \"" + raw + "\"\nscenarios:\n  a:\n    target:\n      pid: 5\n    probe:\n      requests:\n        - url: http://x/\n"
			if _, err := Parse([]byte(input)); err == nil {
				t.Fatalf("expected an error for grace_period %q", raw)
			}
		})
	}
}

func TestDurationRoundTrip(t *testing.T) {
	d := Duration(90 * time.Second)
	got, err := d.MarshalYAML()
	if err != nil {
		t.Fatalf("MarshalYAML: %v", err)
	}
	if got != "1m30s" {
		t.Errorf("MarshalYAML = %v, want 1m30s", got)
	}
	if d.String() != "1m30s" {
		t.Errorf("String() = %q", d.String())
	}
}

func scenarioWith(t *testing.T, body string) error {
	t.Helper()
	_, err := Parse([]byte("version: 1\nscenarios:\n  a:\n" + body))
	return err
}

func TestTargetValidation(t *testing.T) {
	cases := map[string]string{
		"no target": `    target: {}
    probe:
      requests:
        - url: http://x/
`,
		"two targets": `    target:
      pid: 5
      docker: api
    probe:
      requests:
        - url: http://x/
`,
		"bad container ref": `    target:
      docker: "api; rm -rf /"
    probe:
      requests:
        - url: http://x/
`,
		"ready without url or port": `    target:
      command: ["./api"]
      ready: {}
    probe:
      requests:
        - url: http://x/
`,
		"ready with url and port": `    target:
			command: ["./api"]
			ready:
				url: http://x/ready
				port: 8080
		probe:
			requests:
				- url: http://x/
`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if err := scenarioWith(t, body); err == nil {
				t.Fatalf("expected a validation error for %s", name)
			}
		})
	}
}

func TestProbeValidation(t *testing.T) {
	cases := map[string]string{
		"no requests": `    target:
      pid: 5
    probe:
      requests: []
`,
		"missing url": `    target:
      pid: 5
    probe:
      requests:
        - method: GET
`,
		"non-http scheme": `    target:
      pid: 5
    probe:
      requests:
        - url: file:///etc/passwd
`,
		"body and body_file": `    target:
      pid: 5
    probe:
      requests:
        - url: http://x/
          body: hello
          body_file: ./b.json
`,
		"lowercase method": `    target:
      pid: 5
    probe:
      requests:
        - url: http://x/
          method: post
`,
		"invalid method token": `    target:
			pid: 5
		probe:
			requests:
				- url: http://x/
					method: "BAD METHOD"
`,
		"invalid header name": `    target:
			pid: 5
		probe:
			requests:
				- url: http://x/
					headers:
						"Bad Header": value
`,
		"invalid header value": `    target:
			pid: 5
		probe:
			requests:
				- url: http://x/
					headers:
						X-Test: "bad\u0000value"
`,
		"duplicate header case": `    target:
			pid: 5
		probe:
			requests:
				- url: http://x/
					headers:
						X-Trace: first
						x-trace: second
`,
		"all weights zero": `    target:
      pid: 5
    probe:
      requests:
        - url: http://x/a
          weight: 0
        - url: http://x/b
          weight: 0
`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if err := scenarioWith(t, body); err == nil {
				t.Fatalf("expected a validation error for %s", name)
			}
		})
	}
}

func TestURLValidationDoesNotLeakCredentials(t *testing.T) {
	const secret = "config-secret-must-not-appear"
	err := scenarioWith(t, `    target:
      pid: 5
    probe:
      requests:
        - url: ftp://user:`+secret+`@example.test/path?token=`+secret+`
`)
	if err == nil {
		t.Fatal("expected URL validation to fail")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("validation error leaked credentials: %v", err)
	}
}

func TestLoadValidation(t *testing.T) {
	base := `    target:
      pid: 5
    probe:
      requests:
        - url: http://x/
    load:
`
	cases := map[string]string{
		"rps with ensure_in_flight": "      rps: 100\n      ensure_in_flight: 20\n",
		"negative rps":              "      rps: -1\n",
		"not-a-number rps":          "      rps: .nan\n",
		"infinite max rps":          "      max_rps: .inf\n",
		"excessive rps":             "      rps: 1000001\n",
		"rps above max":             "      rps: 5000\n      max_rps: 100\n",
		"excessive concurrency":     "      concurrency_cap: 65537\n",
		"excessive duration":        "      steady: 25h\n",
		"zero warmup":               "      warmup: 0s\n",
	}

	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			if err := scenarioWith(t, base+extra); err == nil {
				t.Fatalf("expected a validation error for %s", name)
			}
		})
	}
}

func TestFixedRPSRejectsSlowURL(t *testing.T) {
	err := scenarioWith(t, `    target:
			pid: 5
		probe:
			slow_url: http://x/slow
			requests:
				- url: http://x/
		load:
			rps: 100
`)
	if err == nil {
		t.Fatal("fixed rps with slow_url was accepted")
	}
}

func TestTerminationValidation(t *testing.T) {
	base := `    target:
      pid: 5
    probe:
      requests:
        - url: http://x/
    termination:
`
	cases := map[string]string{
		"bad signal":           "      signal: HUP\n",
		"bad profile":          "      profile: nonsense\n",
		"window exceeds grace": "      accept_window: 30s\n      grace_period: 10s\n",
	}

	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			if err := scenarioWith(t, base+extra); err == nil {
				t.Fatalf("expected a validation error for %s", name)
			}
		})
	}
}

// A misspelled signature id in `ignore` would silently suppress nothing, and in
// `fail_on` would silently gate nothing. Both must be rejected outright.
func TestGateRejectsUnknownSignatureIDs(t *testing.T) {
	body := `    target:
      pid: 5
    probe:
      requests:
        - url: http://x/
    gate:
      ignore: [SC999]
`
	err := scenarioWith(t, body)
	if err == nil {
		t.Fatal("expected an error for an unknown signature id")
	}
	if !strings.Contains(err.Error(), "SC999") {
		t.Errorf("error should name the offending id, got: %v", err)
	}
}

func TestGateRejectsContradictoryOverrides(t *testing.T) {
	body := `    target:
      pid: 5
    probe:
      requests:
        - url: http://x/
    gate:
      fail_on: [SC009]
      ignore: [SC009]
`
	if err := scenarioWith(t, body); err == nil {
		t.Fatal("expected an error when a signature is both failed on and ignored")
	}
}

func TestGateRangeValidation(t *testing.T) {
	base := `    target:
      pid: 5
    probe:
      requests:
        - url: http://x/
    gate:
`
	cases := map[string]string{
		"drop pct too high":     "      max_inflight_drop_pct: 101\n",
		"drop pct not a number": "      max_inflight_drop_pct: .nan\n",
		"score too high":        "      min_score: 101\n",
		"negative score":        "      min_score: -1\n",
		"zero trials":           "      trials: 0\n",
		"excessive trials":      "      trials: 101\n",
	}

	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			if err := scenarioWith(t, base+extra); err == nil {
				t.Fatalf("expected a validation error for %s", name)
			}
		})
	}
}

// Fixing config one error at a time is miserable, so validation must report
// everything it can see in a single pass.
func TestValidationReportsEveryProblemAtOnce(t *testing.T) {
	input := `
version: 99
scenarios:
  a:
    target: {}
    probe:
      requests:
        - url: not-a-url
    gate:
      min_score: 900
`
	_, err := Parse([]byte(input))
	if err == nil {
		t.Fatal("expected validation errors")
	}

	var problems Problems
	if !errors.As(err, &problems) {
		t.Fatalf("error is not Problems: %T", err)
	}
	if len(problems) < 4 {
		t.Errorf("reported %d problems, want at least 4:\n%v", len(problems), err)
	}
}

func TestValidationComplexityLimits(t *testing.T) {
	t.Run("problem count", func(t *testing.T) {
		var problems Problems
		for i := 0; i < maxValidationProblems*2; i++ {
			problems.add("field", "problem %d", i)
		}
		if len(problems) != maxValidationProblems {
			t.Fatalf("retained %d problems, want %d", len(problems), maxValidationProblems)
		}
		if !strings.Contains(problems[len(problems)-1].Msg, "omitted") {
			t.Errorf("final problem does not explain truncation: %q", problems[len(problems)-1].Msg)
		}
	})

	t.Run("request definitions", func(t *testing.T) {
		requests := make([]RequestSpec, MaxRequestDefinitions+1)
		for i := range requests {
			requests[i] = RequestSpec{URL: "http://x/", Weight: 1}
		}
		file := &File{Version: SchemaVersion, Scenarios: map[string]*Scenario{
			"api": {Target: TargetSpec{PID: 5}, Probe: ProbeSpec{Requests: requests}},
		}}
		if err := file.Validate(); err == nil || !strings.Contains(err.Error(), "definitions") {
			t.Fatalf("request-definition limit error = %v", err)
		}
	})

	t.Run("headers", func(t *testing.T) {
		headers := make(map[string]string, MaxHeadersPerRequest+1)
		for i := 0; i <= MaxHeadersPerRequest; i++ {
			headers[fmt.Sprintf("X-Test-%d", i)] = "value"
		}
		file := &File{Version: SchemaVersion, Scenarios: map[string]*Scenario{
			"api": {Target: TargetSpec{PID: 5}, Probe: ProbeSpec{Requests: []RequestSpec{{URL: "http://x/", Headers: headers}}}},
		}}
		if err := file.Validate(); err == nil || !strings.Contains(err.Error(), "headers") {
			t.Fatalf("header limit error = %v", err)
		}
	})
}

func TestResolveSingleScenarioWithoutName(t *testing.T) {
	file, err := Parse([]byte(specExample))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := file.Resolve(""); err != nil {
		t.Fatalf("Resolve(\"\") with one scenario should succeed: %v", err)
	}
}

func TestResolveRequiresNameWhenAmbiguous(t *testing.T) {
	input := `
version: 1
scenarios:
  a:
    target:
      pid: 5
    probe:
      requests:
        - url: http://x/
  b:
    target:
      pid: 6
    probe:
      requests:
        - url: http://y/
`
	file, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if _, err := file.Resolve(""); err == nil {
		t.Fatal("expected an error when several scenarios exist and none was chosen")
	}
	if _, err := file.Resolve("missing"); err == nil {
		t.Fatal("expected an error for an unknown scenario name")
	}
	if _, err := file.Resolve("b"); err != nil {
		t.Fatalf("Resolve(b): %v", err)
	}
}

func TestResolveAppliesBuiltInDefaults(t *testing.T) {
	input := `
version: 1
scenarios:
  a:
    target:
      pid: 5
    probe:
      requests:
        - url: http://localhost:8080/
`
	file, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	resolved, err := file.Resolve("a")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if resolved.Traffic.EnsureInFlight != DefaultEnsureInFlight {
		t.Errorf("ensure_in_flight = %d, want %d", resolved.Traffic.EnsureInFlight, DefaultEnsureInFlight)
	}
	if resolved.Traffic.Warmup != DefaultWarmup {
		t.Errorf("warmup = %v, want %v", resolved.Traffic.Warmup, DefaultWarmup)
	}
	if !resolved.Traffic.KeepAlive {
		t.Error("keep-alive should default to on")
	}
	if resolved.Termination.Signal != DefaultSignal {
		t.Errorf("signal = %q, want %q", resolved.Termination.Signal, DefaultSignal)
	}
	if resolved.Trials != DefaultTrials {
		t.Errorf("trials = %d, want %d", resolved.Trials, DefaultTrials)
	}
	if got := resolved.Probe.Requests[0].Method; got != DefaultMethod {
		t.Errorf("method = %q, want %q", got, DefaultMethod)
	}
	if got := resolved.Probe.Requests[0].Weight; got != 1 {
		t.Errorf("a lone request must get a usable weight, got %d", got)
	}
}

func TestResolveCanonicalizesAndCopiesHeaders(t *testing.T) {
	file, err := Parse([]byte(`
version: 1
scenarios:
  api:
    target:
      pid: 5
    probe:
      requests:
        - url: http://x/
          headers:
            x-trace-id: original
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	resolved, err := file.Resolve("api")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := resolved.Probe.Requests[0].Headers["X-Trace-Id"]; got != "original" {
		t.Errorf("canonical header = %q, want original", got)
	}
	resolved.Probe.Requests[0].Headers["X-Trace-Id"] = "changed"
	if got := file.Scenarios["api"].Probe.Requests[0].Headers["x-trace-id"]; got != "original" {
		t.Errorf("resolved header mutation changed parsed config to %q", got)
	}
}

// Under `auto`, a bare process must not be judged by Kubernetes rules, and a
// container must inherit docker's shorter grace period.
func TestResolveAutoProfileFollowsTargetKind(t *testing.T) {
	cases := map[string]struct {
		target      string
		wantProfile analyze.Profile
		wantGrace   time.Duration
	}{
		"process": {"      pid: 5\n", analyze.ProfileStandalone, 30 * time.Second},
		"command": {"      command: [\"./api\"]\n", analyze.ProfileStandalone, 30 * time.Second},
		"docker":  {"      docker: api\n", analyze.ProfileDocker, 10 * time.Second},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			input := "version: 1\nscenarios:\n  a:\n    target:\n" + tc.target +
				"    probe:\n      requests:\n        - url: http://x/\n"
			file, err := Parse([]byte(input))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			resolved, err := file.Resolve("a")
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if resolved.Policy.Profile != tc.wantProfile {
				t.Errorf("profile = %q, want %q", resolved.Policy.Profile, tc.wantProfile)
			}
			if resolved.Policy.GracePeriod != tc.wantGrace {
				t.Errorf("grace = %v, want %v", resolved.Policy.GracePeriod, tc.wantGrace)
			}
		})
	}
}

func TestResolveFailOnPromotesSeverity(t *testing.T) {
	input := `
version: 1
scenarios:
  a:
    target:
      pid: 5
    probe:
      requests:
        - url: http://x/
    gate:
      fail_on: [SC009, SC014]
`
	file, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	resolved, err := file.Resolve("a")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	for _, id := range []analyze.SignatureID{analyze.SC009, analyze.SC014} {
		if got := resolved.Policy.SeverityOf(id); got != schema.SeverityError {
			t.Errorf("%s severity = %q, want error", id, got)
		}
	}
}

func TestResolveRejectsIncoherentPolicy(t *testing.T) {
	// lame-duck requires a de-registration window, so an explicit zero window
	// contradicts the profile and must be caught rather than silently applied.
	input := `
version: 1
scenarios:
  a:
    target:
      pid: 5
    probe:
      requests:
        - url: http://x/
    termination:
      profile: lame-duck
      accept_window: 0s
`
	file, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := file.Resolve("a"); err == nil {
		t.Fatal("expected an error for a lame-duck profile with a zero accept window")
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load("testdata/does-not-exist.yaml"); err == nil {
		t.Fatal("expected an error for a missing config file")
	}
}

func TestProblemsErrorFormatting(t *testing.T) {
	single := Problems{{Path: "version", Msg: "missing"}}
	if !strings.Contains(single.Error(), "version: missing") {
		t.Errorf("single problem formatting: %q", single.Error())
	}

	many := Problems{{Path: "a", Msg: "x"}, {Path: "b", Msg: "y"}}
	got := many.Error()
	if !strings.Contains(got, "2 problems") || !strings.Contains(got, "- a: x") {
		t.Errorf("multi problem formatting: %q", got)
	}
}

func TestValidContainerRef(t *testing.T) {
	valid := []string{"api", "my-api", "my_api.1", "a1b2c3d4e5f6"}
	invalid := []string{"", "-leading", ".dot", "_under", "has space", "semi;colon", "quote\"", strings.Repeat("a", 129)}

	for _, s := range valid {
		if !validContainerRef(s) {
			t.Errorf("validContainerRef(%q) = false, want true", s)
		}
	}
	for _, s := range invalid {
		if validContainerRef(s) {
			t.Errorf("validContainerRef(%q) = true, want false", s)
		}
	}
}

func FuzzParse(f *testing.F) {
	f.Add(specExample)
	f.Add("version: 1")
	f.Add("")
	f.Add("version: 1\nscenarios:\n  a:\n    target:\n      pid: 5\n")
	f.Add("defaults:\n  grace_period: 30s\n")
	f.Add("version: 1\nscenarios:\n  a:\n    target:\n      docker: \"\\x00\"\n")

	f.Fuzz(func(t *testing.T, input string) {
		// Config comes from a repository and may be malformed in any way; the
		// parser must always produce an error rather than panic.
		file, err := Parse([]byte(input))
		if err != nil {
			return
		}
		if file == nil {
			t.Fatal("Parse returned nil file and nil error")
		}
		for _, name := range file.ScenarioNames() {
			if _, err := file.Resolve(name); err != nil {
				continue // resolution may legitimately fail on an incoherent policy
			}
		}
	})
}
