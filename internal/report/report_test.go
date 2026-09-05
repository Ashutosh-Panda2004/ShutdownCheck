package report

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shutdowncheck/shutdowncheck/internal/analyze"
	"github.com/shutdowncheck/shutdowncheck/internal/timeline"
	"github.com/shutdowncheck/shutdowncheck/pkg/schema"
)

var update = flag.Bool("update", false, "rewrite golden files")

const (
	signalAt = 5 * time.Second
	exitAt   = 12 * time.Second
)

// brokenRun is a service that fails in several ways at once, so every section
// of every renderer has something to show.
func brokenRun(t *testing.T) (analyze.Result, timeline.Timeline) {
	t.Helper()

	rec := timeline.NewRecorder(timeline.Meta{
		ToolVersion: "1.0.0",
		StartedAt:   time.Date(2026, time.September, 3, 10, 15, 18, 0, time.UTC),
		Target:      "./bin/api --port 8080",
		Profile:     "kubernetes",
		Trial:       1,
		Trials:      1,
	}, 0)

	add := func(e timeline.Event) { rec.Record(e) }

	// Requests are spread across the run rather than bunched at a few instants,
	// so the rendered timeline looks like a real one.
	for i := range 40 {
		sent := time.Second + time.Duration(i)*95*time.Millisecond
		add(timeline.RequestAt(timeline.RequestEvent{
			Method: "POST", URL: "http://localhost:8080/api/orders",
			Sent: sent, Done: sent + 60*time.Millisecond, Status: 200, Outcome: timeline.OutcomeOK,
		}))
	}
	for i := range 16 {
		sent := 4880*time.Millisecond + time.Duration(i)*time.Millisecond
		add(timeline.RequestAt(timeline.RequestEvent{
			Method: "POST", URL: "http://localhost:8080/api/orders",
			Sent: sent, Done: 5100 * time.Millisecond, Status: 200, Outcome: timeline.OutcomeOK,
		}))
	}
	for i := range 6 {
		sent := 4890*time.Millisecond + time.Duration(i)*time.Millisecond
		add(timeline.RequestAt(timeline.RequestEvent{
			Method: "POST", URL: "http://localhost:8080/api/orders",
			Sent: sent, Done: 5050 * time.Millisecond, Outcome: timeline.OutcomeReset,
		}))
	}
	for i := range 24 {
		sent := 5100*time.Millisecond + time.Duration(i)*80*time.Millisecond
		add(timeline.RequestAt(timeline.RequestEvent{
			Method: "POST", URL: "http://localhost:8080/api/orders",
			Sent: sent, Done: sent + 10*time.Millisecond, Outcome: timeline.OutcomeRefused,
		}))
	}

	for at := time.Second; at <= 11*time.Second; at += time.Second {
		add(timeline.ReadinessAt(at, timeline.ReadinessEvent{Status: 200, Healthy: true}))
	}
	add(timeline.ListenerAt(4*time.Second, timeline.ListenerEvent{Accepting: true}))
	add(timeline.ListenerAt(5010*time.Millisecond, timeline.ListenerEvent{Accepting: false, Outcome: timeline.OutcomeRefused}))

	add(timeline.ConnectionAt(time.Second, timeline.ConnectionEvent{ID: 1, Phase: timeline.ConnOpen}))
	add(timeline.ConnectionAt(5500*time.Millisecond, timeline.ConnectionEvent{ID: 1, Phase: timeline.ConnReuse, Reused: true}))
	add(timeline.ConnectionAt(6*time.Second, timeline.ConnectionEvent{ID: 1, Phase: timeline.ConnClose, Termination: timeline.TermRST}))

	add(timeline.SignalAt(signalAt, timeline.SignalEvent{Signal: "TERM"}))
	code := 0
	add(timeline.ProcessAt(exitAt, timeline.ProcessEvent{Phase: timeline.ProcExited, PID: 12345, ExitCode: &code}))

	tl := rec.Snapshot()

	policy, err := analyze.PolicyFor(analyze.ProfileKubernetes)
	if err != nil {
		t.Fatalf("PolicyFor: %v", err)
	}

	result := analyze.Analyze(analyze.Input{
		Timeline:    tl,
		Policy:      policy,
		ToolVersion: "1.0.0",
		Target: analyze.TargetInfo{
			Kind: analyze.TargetCommand, Label: "./bin/api --port 8080",
			PID: 12345, DetectedStack: "dotnet-aspnetcore",
		},
		Probe: analyze.ProbeInfo{
			URL: "http://localhost:8080/api/orders", Method: "POST",
			ReadinessURL: "http://localhost:8080/readyz",
		},
		Load: analyze.LoadInfo{Calibrated: true, RPS: 340, TargetInFlight: 20, Achievable: true},
	})
	return result, tl
}

func healthyRun(t *testing.T) (analyze.Result, timeline.Timeline) {
	t.Helper()

	rec := timeline.NewRecorder(timeline.Meta{ToolVersion: "1.0.0"}, 0)
	for range 20 {
		rec.Record(timeline.RequestAt(timeline.RequestEvent{
			Method: "GET", URL: "http://localhost:8080/", Sent: time.Second,
			Done: 1050 * time.Millisecond, Status: 200, Outcome: timeline.OutcomeOK,
		}))
	}
	for range 12 {
		rec.Record(timeline.RequestAt(timeline.RequestEvent{
			Method: "GET", URL: "http://localhost:8080/", Sent: 4900 * time.Millisecond,
			Done: 5100 * time.Millisecond, Status: 200, Outcome: timeline.OutcomeOK,
		}))
	}
	rec.Record(timeline.SignalAt(signalAt, timeline.SignalEvent{Signal: "TERM"}))
	code := 0
	rec.Record(timeline.ProcessAt(6*time.Second, timeline.ProcessEvent{Phase: timeline.ProcExited, ExitCode: &code}))

	tl := rec.Snapshot()
	policy, err := analyze.PolicyFor(analyze.ProfileStandalone)
	if err != nil {
		t.Fatalf("PolicyFor: %v", err)
	}

	return analyze.Analyze(analyze.Input{
		Timeline: tl, Policy: policy, ToolVersion: "1.0.0",
		Probe: analyze.ProbeInfo{URL: "http://localhost:8080/", Method: "GET"},
	}), tl
}

func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()

	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run: go test ./internal/report -update): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s changed.\nRefresh with: go test ./internal/report -update\n\ngot:\n%s\n\nwant:\n%s",
			name, got, want)
	}
}

func TestHumanGolden(t *testing.T) {
	result, tl := brokenRun(t)

	var buf bytes.Buffer
	if err := Human(&buf, result, tl, Options{Width: 88, NoColor: true}); err != nil {
		t.Fatalf("Human: %v", err)
	}
	checkGolden(t, "human.golden.txt", buf.Bytes())
}

func TestHumanGoldenHealthy(t *testing.T) {
	result, tl := healthyRun(t)

	var buf bytes.Buffer
	if err := Human(&buf, result, tl, Options{Width: 88, NoColor: true}); err != nil {
		t.Fatalf("Human: %v", err)
	}
	checkGolden(t, "human-pass.golden.txt", buf.Bytes())
}

func TestJSONGolden(t *testing.T) {
	result, _ := brokenRun(t)

	var buf bytes.Buffer
	if err := JSON(&buf, result.Report); err != nil {
		t.Fatalf("JSON: %v", err)
	}
	checkGolden(t, "report.golden.json", buf.Bytes())
}

func TestJUnitGolden(t *testing.T) {
	result, _ := brokenRun(t)

	var buf bytes.Buffer
	if err := JUnit(&buf, result); err != nil {
		t.Fatalf("JUnit: %v", err)
	}
	checkGolden(t, "report.golden.xml", buf.Bytes())
}

func TestMarkdownGolden(t *testing.T) {
	result, _ := brokenRun(t)

	var buf bytes.Buffer
	if err := Markdown(&buf, result); err != nil {
		t.Fatalf("Markdown: %v", err)
	}
	checkGolden(t, "report.golden.md", buf.Bytes())
}

func TestMarkdownTargetCannotBreakOutOfCodeSpan(t *testing.T) {
	result, _ := brokenRun(t)
	result.Report.Target.Label = "api` **injected**"

	var buf bytes.Buffer
	if err := Markdown(&buf, result); err != nil {
		t.Fatalf("Markdown: %v", err)
	}
	if !strings.Contains(buf.String(), "`` api` **injected** ``") {
		t.Fatalf("target label was not enclosed in a safe code span:\n%s", buf.String())
	}
}

func TestMarkdownPreservesInformationalSeverity(t *testing.T) {
	result, _ := brokenRun(t)
	result.Report.Findings = []schema.Finding{{
		ID: "SC009", Name: "KEEPALIVE_NOT_TERMINATED", Severity: schema.SeverityInfo,
		Summary: "ignored by policy",
	}}

	var buf bytes.Buffer
	if err := Markdown(&buf, result); err != nil {
		t.Fatalf("Markdown: %v", err)
	}
	if !strings.Contains(buf.String(), "| info |") || strings.Contains(buf.String(), "| warn |") {
		t.Fatalf("Markdown changed informational severity:\n%s", buf.String())
	}
}

func TestBadgeGolden(t *testing.T) {
	result, _ := brokenRun(t)

	var buf bytes.Buffer
	if err := Badge(&buf, result.Report); err != nil {
		t.Fatalf("Badge: %v", err)
	}
	checkGolden(t, "badge.golden.svg", buf.Bytes())
}

// Every renderer is compared across runs and parsed by CI, so identical input
// must always produce identical bytes.
func TestRenderersAreDeterministic(t *testing.T) {
	result, tl := brokenRun(t)

	renderers := map[string]func(*bytes.Buffer) error{
		"human":    func(b *bytes.Buffer) error { return Human(b, result, tl, Options{Width: 88, NoColor: true}) },
		"json":     func(b *bytes.Buffer) error { return JSON(b, result.Report) },
		"junit":    func(b *bytes.Buffer) error { return JUnit(b, result) },
		"markdown": func(b *bytes.Buffer) error { return Markdown(b, result) },
		"badge":    func(b *bytes.Buffer) error { return Badge(b, result.Report) },
		"ndjson":   func(b *bytes.Buffer) error { return NDJSON(b, tl) },
	}

	for name, render := range renderers {
		t.Run(name, func(t *testing.T) {
			var first bytes.Buffer
			if err := render(&first); err != nil {
				t.Fatalf("render: %v", err)
			}

			for range 20 {
				var next bytes.Buffer
				if err := render(&next); err != nil {
					t.Fatalf("render: %v", err)
				}
				if !bytes.Equal(next.Bytes(), first.Bytes()) {
					t.Fatal("renderer output varies between identical inputs")
				}
			}
		})
	}
}

// An inconclusive run must never read as a pass, in any format.
func TestInconclusiveIsNeverPresentedAsSuccess(t *testing.T) {
	rec := timeline.NewRecorder(timeline.Meta{}, 0)
	rec.Record(timeline.RequestAt(timeline.RequestEvent{
		Method: "GET", Sent: 4900 * time.Millisecond, Done: 5100 * time.Millisecond, Outcome: timeline.OutcomeOK,
	}))
	rec.Record(timeline.SignalAt(signalAt, timeline.SignalEvent{Signal: "TERM"}))
	tl := rec.Snapshot()

	policy, err := analyze.PolicyFor(analyze.ProfileStandalone)
	if err != nil {
		t.Fatalf("PolicyFor: %v", err)
	}
	result := analyze.Analyze(analyze.Input{Timeline: tl, Policy: policy})

	if result.Report.Verdict != schema.VerdictInconclusive {
		t.Fatalf("verdict = %q, want inconclusive", result.Report.Verdict)
	}

	var human bytes.Buffer
	if err := Human(&human, result, tl, Options{Width: 88, NoColor: true}); err != nil {
		t.Fatalf("Human: %v", err)
	}
	if !strings.Contains(human.String(), "INCONCLUSIVE") {
		t.Error("the human report must say INCONCLUSIVE plainly")
	}

	var md bytes.Buffer
	if err := Markdown(&md, result); err != nil {
		t.Fatalf("Markdown: %v", err)
	}
	if !strings.Contains(md.String(), "not** a pass") {
		t.Error("the markdown summary must spell out that this is not a pass")
	}

	var junit bytes.Buffer
	if err := JUnit(&junit, result); err != nil {
		t.Fatalf("JUnit: %v", err)
	}
	if strings.Contains(junit.String(), `failures="0"`) ||
		!strings.Contains(junit.String(), `<failure message="Only 1 request(s) were in flight`) {
		t.Errorf("JUnit presented an inconclusive run as passing:\n%s", junit.String())
	}

	var badge bytes.Buffer
	if err := Badge(&badge, result.Report); err != nil {
		t.Fatalf("Badge: %v", err)
	}
	if !strings.Contains(badge.String(), "inconclusive") || strings.Contains(badge.String(), "#4c1") {
		t.Errorf("badge presented an inconclusive run as green:\n%s", badge.String())
	}
}

func TestFormatValidity(t *testing.T) {
	for _, f := range Formats() {
		if !f.Valid() {
			t.Errorf("%q is listed but reports itself invalid", f)
		}
	}
	if Format("html").Valid() {
		t.Error("html is not supported and must not validate")
	}
}

func TestPaletteRespectsNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	p := NewPalette(&bytes.Buffer{}, false, false)
	if p.Enabled() {
		t.Fatal("NO_COLOR must disable styling")
	}
	if got := p.Red("x"); got != "x" {
		t.Errorf("Red(%q) = %q, want it unchanged", "x", got)
	}
}

func TestPaletteDisabledForNonTerminals(t *testing.T) {
	if NewPalette(&bytes.Buffer{}, false, false).Enabled() {
		t.Fatal("styling must be off when writing somewhere that is not a terminal")
	}
	if !NewPalette(&bytes.Buffer{}, true, false).Enabled() {
		t.Fatal("forcing colour should override detection")
	}
	if NewPalette(&bytes.Buffer{}, true, true).Enabled() {
		t.Fatal("an explicit disable must beat an explicit force")
	}
}

func TestThousandsSeparator(t *testing.T) {
	cases := map[int]string{0: "0", 7: "7", 999: "999", 1000: "1,000", 1024: "1,024", 1234567: "1,234,567", -5000: "-5,000"}
	for in, want := range cases {
		if got := thousands(in); got != want {
			t.Errorf("thousands(%d) = %q, want %q", in, got, want)
		}
	}
}
