package cli

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/shutdowncheck/shutdowncheck/internal/analyze"
	"github.com/shutdowncheck/shutdowncheck/internal/config"
	"github.com/shutdowncheck/shutdowncheck/internal/target"
	"github.com/shutdowncheck/shutdowncheck/pkg/schema"
)

func resolveRunFlags(t *testing.T, args ...string) *runFlags {
	t.Helper()

	flags, err := parseRunFlags(args, io.Discard)
	if err != nil {
		t.Fatalf("parseRunFlags: %v", err)
	}
	return flags
}

func overrideConfig(t *testing.T) string {
	t.Helper()

	return writeConfig(t, `
version: 1
defaults:
  profile: kubernetes
  grace_period: 17s
scenarios:
  api:
    target:
      command: ["./api"]
    probe:
      readiness_url: http://localhost:8080/readyz
      requests:
        - url: http://localhost:8080/work
          method: GET
          headers:
            X-Config: present
    load:
      ensure_in_flight: 5
      max_rps: 1000
      concurrency_cap: 20
      warmup: 4s
      steady: 5s
      request_timeout: 6s
      keep_alive: true
    termination:
      accept_window: 2s
    gate:
      ignore: [SC006]
`)
}

// A profile changes profile defaults; it must not erase values the user
// explicitly wrote in the scenario.
func TestProfileOverridePreservesExplicitConfigValues(t *testing.T) {
	flags := resolveRunFlags(t, "--config", overrideConfig(t), "--profile", "standalone")
	resolved, err := flags.resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if resolved.Policy.Profile != analyze.ProfileStandalone {
		t.Errorf("profile = %q", resolved.Policy.Profile)
	}
	if resolved.Policy.GracePeriod != 17*time.Second {
		t.Errorf("grace = %s, want explicit config value 17s", resolved.Policy.GracePeriod)
	}
	if resolved.Policy.AcceptWindow != 2*time.Second {
		t.Errorf("accept window = %s, want explicit config value 2s", resolved.Policy.AcceptWindow)
	}
	if got := resolved.Policy.SeverityOf(analyze.SC006); got != schema.SeverityInfo {
		t.Errorf("SC006 severity = %q, want config override info", got)
	}
}

func TestConfiguredCommandSecretsAreRedactedFromTheTargetLabel(t *testing.T) {
	const secret = "must-not-reach-the-report"
	path := writeConfig(t, `
version: 1
scenarios:
  api:
    target:
      command: ["./api", "--token", "`+secret+`"]
    probe:
      requests:
        - url: http://localhost:8080/work
`)

	flags := resolveRunFlags(t, "--config", path)
	resolved, err := flags.resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if strings.Contains(resolved.Target.Label, secret) {
		t.Errorf("secret reached target label: %q", resolved.Target.Label)
	}
	if !strings.Contains(resolved.Target.Label, "REDACTED") {
		t.Errorf("target label does not show that a value was redacted: %q", resolved.Target.Label)
	}
}

func TestExplicitFlagsOverrideSelectedScenario(t *testing.T) {
	flags := resolveRunFlags(t,
		"--config", overrideConfig(t),
		"--url", "https://example.test/override",
		"--method", "post",
		"--header", "X-Config: replaced",
		"--header", "X-CLI: present",
		"--body", "payload",
		"--readiness-url", "https://example.test/ready",
		"--slow-url", "https://example.test/slow",
		"--insecure",
		"--ensure-in-flight", "7",
		"--max-rps", "99",
		"--concurrency-cap", "11",
		"--warmup", "1s",
		"--steady", "2s",
		"--request-timeout", "3s",
		"--keep-alive=false",
		"--signal", "INT",
		"--prestop-sleep", "4s",
		"--grace-period", "10s",
		"--enforce-sigkill=false",
		"--accept-window", "1s",
		"--max-inflight-drop-pct", "2",
		"--max-shutdown-time", "8s",
		"--min-score", "80",
		"--trials", "2",
		"--capture-target-logs",
	)
	resolved, err := flags.resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	request := resolved.Probe.Requests[0]
	if request.URL != "https://example.test/override" || request.Method != "POST" || request.Body != "payload" {
		t.Errorf("request override = %+v", request)
	}
	if request.Headers["X-Config"] != "replaced" || request.Headers["X-Cli"] != "present" {
		t.Errorf("headers = %v", request.Headers)
	}
	if resolved.Probe.ReadinessURL != "https://example.test/ready" ||
		resolved.Probe.SlowURL != "https://example.test/slow" || !resolved.Probe.Insecure {
		t.Errorf("probe overrides = %+v", resolved.Probe)
	}
	if !resolved.Traffic.Calibrate || resolved.Traffic.EnsureInFlight != 7 ||
		resolved.Traffic.MaxRPS != 99 || resolved.Traffic.ConcurrencyCap != 11 ||
		resolved.Traffic.Warmup != time.Second || resolved.Traffic.Steady != 2*time.Second ||
		resolved.Traffic.RequestTimeout != 3*time.Second || resolved.Traffic.KeepAlive {
		t.Errorf("traffic overrides = %+v", resolved.Traffic)
	}
	if resolved.Termination.Signal != "INT" || resolved.Termination.PreStopSleep != 4*time.Second ||
		resolved.Termination.GracePeriod != 10*time.Second || resolved.Termination.EnforceSigkill {
		t.Errorf("termination overrides = %+v", resolved.Termination)
	}
	if resolved.Policy.AcceptWindow != time.Second || resolved.Policy.MaxInFlightDropPct != 2 ||
		resolved.Policy.MaxShutdownTime == nil || *resolved.Policy.MaxShutdownTime != 8*time.Second ||
		resolved.Policy.MinScore == nil || *resolved.Policy.MinScore != 80 {
		t.Errorf("policy overrides = %+v", resolved.Policy)
	}
	if resolved.Trials != 2 || !resolved.CaptureTargetLogs {
		t.Errorf("trials/log overrides = %d/%v", resolved.Trials, resolved.CaptureTargetLogs)
	}
}

func TestTargetOverrideReResolvesAutoProfile(t *testing.T) {
	flags := resolveRunFlags(t, "--config", overrideConfig(t), "--docker", "api")
	resolved, err := flags.resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if resolved.Target.Kind != analyze.TargetDocker {
		t.Errorf("target kind = %q", resolved.Target.Kind)
	}
	// The config explicitly chooses kubernetes rather than auto, so its profile
	// remains. A separate auto-profile case verifies target-derived behavior.
	if resolved.Policy.Profile != analyze.ProfileKubernetes {
		t.Errorf("explicit profile changed to %q", resolved.Policy.Profile)
	}

	auto := writeConfig(t, `
version: 1
scenarios:
  api:
    target:
      command: ["./api"]
    probe:
      requests:
        - url: http://localhost:8080/work
`)
	flags = resolveRunFlags(t, "--config", auto, "--docker", "api")
	resolved, err = flags.resolve()
	if err != nil {
		t.Fatalf("resolve auto: %v", err)
	}
	if resolved.Policy.Profile != analyze.ProfileDocker || resolved.Policy.GracePeriod != 10*time.Second {
		t.Errorf("auto docker policy = %q/%s", resolved.Policy.Profile, resolved.Policy.GracePeriod)
	}
}

func TestAllowUnsafePIDFlagIsUsableOnlyForPIDTargets(t *testing.T) {
	flags := resolveRunFlags(t, "--config", overrideConfig(t), "--pid", "1", "--allow-unsafe-pid")
	resolved, err := flags.resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !resolved.Target.AllowUnsafePID {
		t.Error("--allow-unsafe-pid was accepted but not applied")
	}

	flags = resolveRunFlags(t, "--config", overrideConfig(t), "--allow-unsafe-pid")
	if _, err := flags.resolve(); err == nil {
		t.Error("--allow-unsafe-pid without a PID target was accepted")
	}
}

func TestConfiguredPIDOneRequiresExplicitOptIn(t *testing.T) {
	path := writeConfig(t, `
version: 1
scenarios:
  init:
    target:
      pid: 1
    probe:
      requests:
        - url: http://localhost:8080/work
`)

	flags := resolveRunFlags(t, "--config", path)
	if _, err := flags.resolve(); err == nil {
		t.Fatal("configured PID 1 was accepted without --allow-unsafe-pid")
	}

	flags = resolveRunFlags(t, "--config", path, "--allow-unsafe-pid")
	resolved, err := flags.resolve()
	if err != nil {
		t.Fatalf("resolve with opt-in: %v", err)
	}
	if resolved.Target.PID != 1 || !resolved.Target.AllowUnsafePID {
		t.Errorf("target = %+v", resolved.Target)
	}
}

func TestDockerPathOnlyURLsSurviveUntilPortResolution(t *testing.T) {
	flags := resolveRunFlags(t,
		"--docker", "api",
		"--url", "/work",
		"--readiness-url", "/readyz",
		"--slow-url", "/slow",
	)
	resolved, err := flags.resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if got := resolved.Probe.Requests[0].URL; got != "/work" {
		t.Errorf("request URL = %q", got)
	}
	if resolved.Probe.ReadinessURL != "/readyz" || resolved.Probe.SlowURL != "/slow" {
		t.Errorf("probe URLs = %+v", resolved.Probe)
	}
}

func TestDockerResolutionMapsProbeAndReadyPortsFromSameContainer(t *testing.T) {
	containerID := strings.Repeat("a", 64)
	resolved := &config.Resolved{
		Target: config.Target{
			Kind: analyze.TargetDocker, Docker: "api", Ready: config.Ready{Port: 9090},
		},
		Probe: config.Probe{Requests: []config.Request{{URL: "/work"}}},
	}
	flags := &runFlags{containerPort: "8080"}
	state := target.ContainerState{
		ID: containerID,
		Ports: map[string][]target.PortBinding{
			"8080/tcp": {{HostIP: "0.0.0.0", HostPort: "32768"}},
			"9090/tcp": {{HostIP: "0.0.0.0", HostPort: "32769"}},
		},
	}

	if err := applyDockerResolution(flags, resolved, state, true); err != nil {
		t.Fatalf("applyDockerResolution: %v", err)
	}
	if resolved.Target.Docker != containerID {
		t.Errorf("target ref = %q, want immutable ID", resolved.Target.Docker)
	}
	if resolved.Target.Ready.Addr != "127.0.0.1:32769" || resolved.Target.Ready.Port != 0 {
		t.Errorf("ready = %+v, want mapped host address", resolved.Target.Ready)
	}
	if got := resolved.Probe.Requests[0].URL; got != "http://127.0.0.1:32768/work" {
		t.Errorf("request URL = %q", got)
	}
}

func TestDockerResolutionPinsIdentityWithoutPortMapping(t *testing.T) {
	containerID := strings.Repeat("b", 64)
	resolved := &config.Resolved{
		Target: config.Target{Kind: analyze.TargetDocker, Docker: "api"},
		Probe:  config.Probe{Requests: []config.Request{{URL: "https://example.test/work"}}},
	}
	if err := applyDockerResolution(&runFlags{}, resolved, target.ContainerState{ID: containerID}, false); err != nil {
		t.Fatalf("applyDockerResolution: %v", err)
	}
	if resolved.Target.Docker != containerID {
		t.Errorf("target ref = %q, want immutable ID", resolved.Target.Docker)
	}
}

func TestInvalidExplicitRunValuesAreRejected(t *testing.T) {
	path := overrideConfig(t)
	cases := map[string][]string{
		"negative record limit":    {"--max-records", "-1"},
		"excessive record limit":   {"--max-records", "250001"},
		"tiny observe interval":    {"--observe-interval", "1ns"},
		"negative timeout":         {"--timeout", "-1s"},
		"zero fixed rate":          {"--rps", "0"},
		"not-a-number rate":        {"--rps", "NaN"},
		"infinite max rate":        {"--max-rps", "+Inf"},
		"excessive fixed rate":     {"--rps", "1000001", "--max-rps", "1000001"},
		"excessive concurrency":    {"--concurrency-cap", "65537"},
		"not-a-number drop pct":    {"--max-inflight-drop-pct", "NaN"},
		"excessive steady":         {"--steady", "25h"},
		"excessive trials":         {"--trials", "101"},
		"excessive timeout":        {"--timeout", "721h"},
		"excessive width":          {"--width", "1001"},
		"zero in-flight goal":      {"--ensure-in-flight", "0"},
		"conflicting load mode":    {"--rps", "10", "--ensure-in-flight", "5"},
		"fixed rate with slow URL": {"--rps", "10", "--slow-url", "http://localhost:8080/slow"},
		"conflicting severity":     {"--ignore", "SC006", "--fail-on", "SC006"},
		"bad URL scheme":           {"--url", "file:///etc/passwd"},
		"invalid container port":   {"--docker", "api", "--container-port", "53/udp"},
	}

	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			args := append([]string{"--config", path}, extra...)
			flags := resolveRunFlags(t, args...)
			if _, err := flags.resolve(); err == nil {
				t.Fatal("invalid explicit values were accepted")
			}
		})
	}
}

func TestCLIURLValidationDoesNotLeakCredentials(t *testing.T) {
	const secret = "cli-secret-must-not-appear"
	flags := resolveRunFlags(t,
		"--pid", "42",
		"--url", "ftp://user:"+secret+"@example.test/path?token="+secret,
	)
	_, err := flags.resolve()
	if err == nil {
		t.Fatal("expected URL validation to fail")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("validation error leaked credentials: %v", err)
	}
}
