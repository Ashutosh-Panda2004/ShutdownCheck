package analyze

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/shutdowncheck/shutdowncheck/internal/timeline"
	"github.com/shutdowncheck/shutdowncheck/pkg/schema"
)

// The boundaries decide which bucket a request lands in, and the in-flight
// bucket is what the entire verdict turns on. A request that started exactly at
// the signal was not yet being processed, and one that finished exactly then was
// already done; counting either as in-flight would inflate the evidence.
func TestPhaseClassificationBoundaries(t *testing.T) {
	policy := lameDuckPolicy(t)

	cases := map[string]struct {
		sent, done time.Duration
		warmup     bool
		want       Phase
	}{
		"finished before the signal":  {4 * time.Second, 4900 * time.Millisecond, false, PhaseSteady},
		"finished exactly at signal":  {4 * time.Second, fxSignal, false, PhaseSteady},
		"spans the signal":            {4900 * time.Millisecond, 5100 * time.Millisecond, false, PhaseInFlight},
		"sent exactly at signal":      {fxSignal, 5100 * time.Millisecond, false, PhasePostSignal},
		"sent inside the window":      {7 * time.Second, 7100 * time.Millisecond, false, PhasePostSignal},
		"sent at the window edge":     {fxWindowEnd, 10100 * time.Millisecond, false, PhasePostSignal},
		"sent just past the window":   {fxWindowEnd + time.Millisecond, 10200 * time.Millisecond, false, PhasePostWindow},
		"warmup wins over everything": {4900 * time.Millisecond, 5100 * time.Millisecond, true, PhaseWarmup},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture()
			if tc.warmup {
				f.warmup(1, tc.sent, tc.done)
			} else {
				f.requests(1, tc.sent, tc.done, timeline.OutcomeOK)
			}
			f.signal(fxSignal)

			facts := BuildFacts(f.build(), policy)
			if got := facts.Requests[0].Phase; got != tc.want {
				t.Fatalf("phase = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClassificationWithoutASignal(t *testing.T) {
	f := newFixture().requests(3, time.Second, 1100*time.Millisecond, timeline.OutcomeOK)

	facts := BuildFacts(f.build(), lameDuckPolicy(t))
	if facts.HasSignal {
		t.Fatal("no signal was recorded")
	}
	for _, req := range facts.Requests {
		if req.Phase != PhaseSteady {
			t.Errorf("phase = %q, want steady when there is no signal", req.Phase)
		}
	}
}

func TestAtSigkillIsAFlagNotAPhase(t *testing.T) {
	f := healthyRun()
	f.kill(35 * time.Second)
	f.requests(2, 34*time.Second, 36*time.Second, timeline.OutcomeReset)

	facts := BuildFacts(f.build(), lameDuckPolicy(t))
	if facts.AtSigkill != 2 {
		t.Fatalf("AtSigkill = %d, want 2", facts.AtSigkill)
	}

	for _, req := range facts.Requests {
		if req.AtSigkill && req.Phase != PhasePostWindow {
			t.Errorf("a request killed mid-flight lost its original phase: %q", req.Phase)
		}
	}
}

// An unconfigured probe must never be read as evidence of good or bad
// behaviour; absence of data is not data.
func TestMissingProbesAreNotTreatedAsEvidence(t *testing.T) {
	f := newFixture().
		requests(10, 4900*time.Millisecond, 5100*time.Millisecond, timeline.OutcomeOK).
		signal(fxSignal).
		exit(fxExit, 0)

	facts := BuildFacts(f.build(), lameDuckPolicy(t))

	if facts.HadReadinessProbe || facts.HadListenerProbe {
		t.Fatal("no probes were recorded, yet the facts claim otherwise")
	}

	result := Analyze(Input{Timeline: f.build(), Policy: lameDuckPolicy(t)})
	for _, finding := range result.Findings {
		if finding.ID == SC007 {
			t.Error("SC007 must not fire when readiness was never probed")
		}
	}
}

// Inconclusive outranks everything. A run that proved nothing must never be
// reported as a pass.
func TestInsufficientInFlightForcesInconclusive(t *testing.T) {
	f := newFixture().
		requests(1, 4900*time.Millisecond, 5100*time.Millisecond, timeline.OutcomeOK).
		signal(fxSignal).
		exit(fxExit, 0)

	result := Analyze(Input{Timeline: f.build(), Policy: lameDuckPolicy(t)})

	if result.Report.Verdict != schema.VerdictInconclusive {
		t.Fatalf("verdict = %q, want inconclusive", result.Report.Verdict)
	}
	if result.Report.Verdict.ExitCode() != schema.ExitInconclusive {
		t.Errorf("exit code = %d, want %d", result.Report.Verdict.ExitCode(), schema.ExitInconclusive)
	}
}

func TestInconclusiveWinsOverFailingSignatures(t *testing.T) {
	// A dropped in-flight request, but far too few to conclude from.
	f := newFixture().
		requests(1, 4900*time.Millisecond, 5100*time.Millisecond, timeline.OutcomeReset).
		signal(fxSignal).
		exit(fxExit, 0)

	result := Analyze(Input{Timeline: f.build(), Policy: lameDuckPolicy(t)})
	if result.Report.Verdict != schema.VerdictInconclusive {
		t.Fatalf("verdict = %q, want inconclusive even though a signature fired", result.Report.Verdict)
	}
}

func TestErrorSeverityFailsTheRun(t *testing.T) {
	f := newFixture().
		requests(6, 4900*time.Millisecond, 5100*time.Millisecond, timeline.OutcomeOK).
		requests(4, 4900*time.Millisecond, 5050*time.Millisecond, timeline.OutcomeReset).
		signal(fxSignal).
		exit(fxExit, 0)

	result := Analyze(Input{Timeline: f.build(), Policy: lameDuckPolicy(t)})
	if result.Report.Verdict != schema.VerdictFail {
		t.Fatalf("verdict = %q, want fail", result.Report.Verdict)
	}
}

// Demoting a signature to a warning must change the verdict, which is what
// --ignore is for.
func TestIgnoringASignatureDowngradesTheVerdict(t *testing.T) {
	f := newFixture().
		requests(6, 4900*time.Millisecond, 5100*time.Millisecond, timeline.OutcomeOK).
		requests(4, 4900*time.Millisecond, 5050*time.Millisecond, timeline.OutcomeReset).
		signal(fxSignal).
		exit(fxExit, 0)

	policy := lameDuckPolicy(t).WithSeverity(SC003, schema.SeverityWarn)
	policy.MaxInFlightDropPct = 100 // the gate would otherwise still fail the run

	result := Analyze(Input{Timeline: f.build(), Policy: policy})
	if result.Report.Verdict == schema.VerdictFail {
		t.Fatalf("verdict = %q, want the run to pass once SC003 is demoted", result.Report.Verdict)
	}
}

func TestGatesFailTheRunIndependently(t *testing.T) {
	policy := lameDuckPolicy(t)
	score := 100
	policy.MinScore = &score

	// A clean run that simply cannot reach a perfect score requirement.
	result := Analyze(Input{Timeline: healthyRun().build(), Policy: policy})
	if result.Report.Verdict != schema.VerdictPass {
		t.Fatalf("verdict = %q, want pass; the healthy run scores 100", result.Report.Verdict)
	}

	impossible := 101
	policy.MinScore = &impossible
	result = Analyze(Input{Timeline: healthyRun().build(), Policy: policy})
	if result.Report.Verdict != schema.VerdictFail {
		t.Fatalf("verdict = %q, want fail when the score gate cannot be met", result.Report.Verdict)
	}
}

// A process that never exited cannot have satisfied a shutdown budget.
func TestShutdownBudgetGateFailsWhenProcessNeverExited(t *testing.T) {
	policy := lameDuckPolicy(t)
	budget := 30 * time.Second
	policy.MaxShutdownTime = &budget

	f := newFixture().
		requests(10, 4900*time.Millisecond, 5100*time.Millisecond, timeline.OutcomeOK).
		signal(fxSignal)

	result := Analyze(Input{Timeline: f.build(), Policy: policy})

	var found bool
	for _, gate := range result.Report.Gates {
		if gate.Name == "max_shutdown_time_ms" {
			found = true
			if gate.Passed {
				t.Error("the shutdown budget gate passed even though the process never exited")
			}
		}
	}
	if !found {
		t.Error("the shutdown budget gate was not evaluated")
	}
}

// The same evidence judged under a different profile must reach a different
// conclusion; that is the whole point of profiles.
func TestProfileChangesTheVerdictForTheSameEvidence(t *testing.T) {
	instantClose := func() timeline.Timeline {
		f := newFixture()
		f.requests(10, 4900*time.Millisecond, 5100*time.Millisecond, timeline.OutcomeOK)
		f.listener(4*time.Second, true)
		f.listener(5010*time.Millisecond, false)
		f.signal(fxSignal)
		f.exit(fxExit, 0)
		return f.build()
	}

	lameDuck := Analyze(Input{Timeline: instantClose(), Policy: lameDuckPolicy(t)})
	if lameDuck.Report.Verdict != schema.VerdictFail {
		t.Fatalf("lame-duck verdict = %q, want fail: closing instantly causes 502s behind a load balancer",
			lameDuck.Report.Verdict)
	}

	strictPolicy, err := PolicyFor(ProfileStrict)
	if err != nil {
		t.Fatalf("PolicyFor: %v", err)
	}
	strict := Analyze(Input{Timeline: instantClose(), Policy: strictPolicy})
	if strict.Report.Verdict == schema.VerdictFail {
		t.Fatalf("strict verdict = %q, want pass: closing instantly is the requirement here",
			strict.Report.Verdict)
	}
}

// Reports are compared across runs and parsed by CI, so identical evidence must
// always serialise to identical bytes.
func TestAnalysisIsDeterministic(t *testing.T) {
	build := func() Input {
		return Input{
			Timeline:    brokenRun().build(),
			Policy:      lameDuckPolicy(t),
			ToolVersion: "test",
			Target:      TargetInfo{Kind: TargetCommand, Label: "./api"},
			Probe:       ProbeInfo{URL: "http://x/", Method: "GET"},
		}
	}

	first, err := json.Marshal(Analyze(build()).Report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	for i := range 50 {
		next, err := json.Marshal(Analyze(build()).Report)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(next) != string(first) {
			t.Fatalf("analysis differed on iteration %d; verdicts would not be reproducible", i)
		}
	}
}

// brokenRun trips many signatures at once, so the determinism check has plenty
// of findings and evidence to order.
func brokenRun() *fixture {
	f := newFixture()

	f.requests(10, time.Second, 1010*time.Millisecond, timeline.OutcomeOK)
	f.requests(6, 4900*time.Millisecond, 5100*time.Millisecond, timeline.OutcomeOK)
	f.requests(6, 4900*time.Millisecond, 5050*time.Millisecond, timeline.OutcomeReset)
	f.requests(5, 6*time.Second, 6100*time.Millisecond, timeline.OutcomeRefused)
	f.requests(3, 6500*time.Millisecond, 9*time.Second, timeline.OutcomeTimeout)

	f.readiness(time.Second, true)
	f.readiness(6*time.Second, true)
	f.listener(4*time.Second, true)
	f.listener(5010*time.Millisecond, false)

	f.connOpen(time.Second, 1)
	f.connReuse(6*time.Second, 1)
	f.connClose(7*time.Second, 1, timeline.TermRST, false)

	f.signal(fxSignal)
	f.kill(35 * time.Second)
	f.exitBySignal(35100*time.Millisecond, "SIGKILL")
	return f
}

func TestScoreFallsWithSeverity(t *testing.T) {
	healthy := Analyze(Input{Timeline: healthyRun().build(), Policy: lameDuckPolicy(t)})
	broken := Analyze(Input{Timeline: brokenRun().build(), Policy: lameDuckPolicy(t)})

	if healthy.Score.Value != 100 {
		t.Errorf("healthy score = %d, want 100", healthy.Score.Value)
	}
	if broken.Score.Value >= 60 {
		t.Errorf("broken score = %d, want a failing grade", broken.Score.Value)
	}
	if broken.Score.Grade != "F" {
		t.Errorf("broken grade = %q, want F", broken.Score.Grade)
	}
	if len(broken.Score.Deductions) == 0 {
		t.Error("a reduced score must come with its derivation")
	}
}

func TestScoreNeverGoesNegative(t *testing.T) {
	result := Analyze(Input{Timeline: brokenRun().build(), Policy: lameDuckPolicy(t)})
	if result.Score.Value < 0 {
		t.Fatalf("score = %d, want it floored at 0", result.Score.Value)
	}
}

// Losing one request in twenty is not the same defect as losing all of them.
func TestScoreScalesWithHowMuchTrafficWasLost(t *testing.T) {
	run := func(ok, failed int) int {
		f := newFixture()
		f.requests(ok, 4900*time.Millisecond, 5100*time.Millisecond, timeline.OutcomeOK)
		f.requests(failed, 4900*time.Millisecond, 5050*time.Millisecond, timeline.OutcomeReset)
		f.signal(fxSignal)
		f.exit(fxExit, 0)

		policy := lameDuckPolicy(t)
		policy.MaxInFlightDropPct = 100
		return Analyze(Input{Timeline: f.build(), Policy: policy}).Score.Value
	}

	light := run(19, 1)
	heavy := run(1, 19)

	if light <= heavy {
		t.Fatalf("losing 1 of 20 scored %d and losing 19 of 20 scored %d; the worse defect must score lower",
			light, heavy)
	}
}

func TestReportShapeMatchesTheEvidence(t *testing.T) {
	result := Analyze(Input{
		Timeline:    healthyRun().build(),
		Policy:      lameDuckPolicy(t),
		ToolVersion: "1.2.3",
		Target:      TargetInfo{Kind: TargetCommand, Label: "./api", PID: 42, DetectedStack: "go-net-http"},
		Probe:       ProbeInfo{URL: "http://localhost:8080/x", Method: "POST", ReadinessURL: "http://localhost:8080/readyz"},
		Load:        LoadInfo{Calibrated: true, RPS: 340, TargetInFlight: 20},
	})
	report := result.Report

	if report.SchemaVersion != schema.SchemaVersion || report.ToolVersion != "1.2.3" {
		t.Errorf("version fields = %q / %q", report.SchemaVersion, report.ToolVersion)
	}
	if report.Target.Label != "./api" || report.Target.PID != 42 {
		t.Errorf("target = %+v", report.Target)
	}
	if report.Probe.Method != "POST" {
		t.Errorf("probe = %+v", report.Probe)
	}
	if report.Load.ObservedInFlightAtSignal != 10 {
		t.Errorf("observed in-flight = %d, want 10", report.Load.ObservedInFlightAtSignal)
	}
	if report.Requests.ByPhase.InFlight.Count != 10 {
		t.Errorf("in-flight count = %d, want 10", report.Requests.ByPhase.InFlight.Count)
	}
	if report.Requests.ByPhase.Warmup.Count != 5 {
		t.Errorf("warmup count = %d, want 5", report.Requests.ByPhase.Warmup.Count)
	}
	if report.Timeline.ProcessExitedAtMS == nil || *report.Timeline.ProcessExitedAtMS != 7000 {
		t.Errorf("exit offset = %v, want 7000ms after the signal", report.Timeline.ProcessExitedAtMS)
	}
	if report.Timeline.SigkillSentAtMS != nil {
		t.Error("no SIGKILL was sent, so the field must be null")
	}
	if report.Connections.ClosedByFIN != 1 {
		t.Errorf("FIN closes = %d, want 1", report.Connections.ClosedByFIN)
	}
}

// A readiness endpoint that never flipped must serialise as null, not zero:
// "flipped instantly" and "never flipped" are opposite verdicts.
func TestNeverFlippedReadinessIsNull(t *testing.T) {
	f := newFixture().
		requests(10, 4900*time.Millisecond, 5100*time.Millisecond, timeline.OutcomeOK).
		readiness(time.Second, true).
		readiness(6*time.Second, true).
		signal(fxSignal).
		exit(fxExit, 0)

	result := Analyze(Input{Timeline: f.build(), Policy: lameDuckPolicy(t)})
	if result.Report.Timeline.ReadinessFlippedAtMS != nil {
		t.Fatalf("readiness_flipped_at_ms = %v, want null", *result.Report.Timeline.ReadinessFlippedAtMS)
	}
}

func TestAggregateRequiresResults(t *testing.T) {
	if _, err := Aggregate(nil); err == nil {
		t.Fatal("aggregating nothing should fail")
	}
}

func TestAggregateReportsWorstVerdict(t *testing.T) {
	pass := Analyze(Input{Timeline: healthyRun().build(), Policy: lameDuckPolicy(t)})
	fail := Analyze(Input{Timeline: brokenRun().build(), Policy: lameDuckPolicy(t)})

	got, err := Aggregate([]Result{pass, pass, fail})
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}

	if got.Report.Verdict != schema.VerdictFail {
		t.Errorf("verdict = %q, want fail; one failing trial is a real finding", got.Report.Verdict)
	}
	if got.Report.Run.Trials.Total != 3 || got.Report.Run.Trials.Failed != 1 {
		t.Errorf("trials = %+v, want 3 total and 1 failed", got.Report.Run.Trials)
	}
	if got.Report.Run.Trials.Consistent {
		t.Error("a 1-in-3 failure is inconsistent and must be reported as such")
	}
}

func TestAggregateMarksConsistentRuns(t *testing.T) {
	pass := Analyze(Input{Timeline: healthyRun().build(), Policy: lameDuckPolicy(t)})

	got, err := Aggregate([]Result{pass, pass, pass})
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if !got.Report.Run.Trials.Consistent || got.Report.Verdict != schema.VerdictPass {
		t.Errorf("trials = %+v, verdict = %q", got.Report.Run.Trials, got.Report.Verdict)
	}
}

// Positive evidence of a defect outranks the absence of evidence.
func TestAggregatePrefersFailureOverInconclusive(t *testing.T) {
	inconclusive := Analyze(Input{
		Timeline: newFixture().requests(1, 4900*time.Millisecond, 5100*time.Millisecond, timeline.OutcomeOK).
			signal(fxSignal).exit(fxExit, 0).build(),
		Policy: lameDuckPolicy(t),
	})
	fail := Analyze(Input{Timeline: brokenRun().build(), Policy: lameDuckPolicy(t)})

	got, err := Aggregate([]Result{inconclusive, fail})
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if got.Report.Verdict != schema.VerdictFail {
		t.Errorf("verdict = %q, want fail", got.Report.Verdict)
	}
}

// A pass can never override an inconclusive trial.
func TestAggregateWillNotPassWhenATrialProvedNothing(t *testing.T) {
	pass := Analyze(Input{Timeline: healthyRun().build(), Policy: lameDuckPolicy(t)})
	inconclusive := Analyze(Input{
		Timeline: newFixture().requests(1, 4900*time.Millisecond, 5100*time.Millisecond, timeline.OutcomeOK).
			signal(fxSignal).exit(fxExit, 0).build(),
		Policy: lameDuckPolicy(t),
	})

	got, err := Aggregate([]Result{pass, inconclusive, pass})
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if got.Report.Verdict != schema.VerdictInconclusive {
		t.Errorf("verdict = %q, want inconclusive", got.Report.Verdict)
	}
}

// The weight table is only useful if it lands services in sensible bands:
// obviously broken ones in F, one minor flaw still in A or B, and a service
// with a real but single defect in between. This pins that calibration so a
// later tweak to the weights cannot silently rescale every published score.
func TestScoreBandsAreCalibrated(t *testing.T) {
	policy := lameDuckPolicy(t)

	cases := map[string]struct {
		run       func() *fixture
		wantGrade string
	}{
		"correct shutdown": {healthyRun, "A"},
		"one minor flaw": {func() *fixture {
			f := healthyRun()
			// Keep-alive sockets never told to close: real, but small.
			f.events = nil
			f.requests(10, 4900*time.Millisecond, 5100*time.Millisecond, timeline.OutcomeOK)
			f.connOpen(time.Second, 1)
			f.connReuse(6*time.Second, 1)
			f.connClose(11*time.Second, 1, timeline.TermFIN, false)
			return f.signal(fxSignal).exit(fxExit, 0)
		}, "A"},
		"closes too early": {func() *fixture {
			f := newFixture()
			f.requests(10, 4900*time.Millisecond, 5100*time.Millisecond, timeline.OutcomeOK)
			f.listener(4*time.Second, true)
			f.listener(5010*time.Millisecond, false)
			return f.signal(fxSignal).exit(fxExit, 0)
		}, "B"},
		"thoroughly broken": {brokenRun, "F"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			result := Analyze(Input{Timeline: tc.run().build(), Policy: policy})
			if result.Score.Grade != tc.wantGrade {
				t.Errorf("grade = %q (score %d), want %q. deductions: %+v",
					result.Score.Grade, result.Score.Value, tc.wantGrade, result.Score.Deductions)
			}
		})
	}
}

func TestPercentilesUseObservedValues(t *testing.T) {
	got := percentilesOf([]time.Duration{
		10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond,
		40 * time.Millisecond, 100 * time.Millisecond,
	})

	if got.Count != 5 {
		t.Fatalf("count = %d, want 5", got.Count)
	}
	if got.P50 != 30*time.Millisecond {
		t.Errorf("p50 = %v, want 30ms", got.P50)
	}
	if got.P99 != 100*time.Millisecond {
		t.Errorf("p99 = %v, want 100ms", got.P99)
	}
}

func TestPercentilesOfNothing(t *testing.T) {
	if got := percentilesOf(nil); got.Count != 0 || got.P50 != 0 {
		t.Fatalf("percentilesOf(nil) = %+v", got)
	}
}
