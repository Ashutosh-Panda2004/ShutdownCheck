package analyze

import (
	"sort"
	"time"

	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/timeline"
	"github.com/Ashutosh-Panda2004/ShutdownCheck/pkg/schema"
)

// TargetInfo describes what was terminated. It is passed in rather than derived
// because the timeline records observations, not configuration.
type TargetInfo struct {
	Kind          TargetKind
	Label         string
	PID           int
	DetectedStack string
}

// ProbeInfo describes what was requested.
type ProbeInfo struct {
	URL          string
	Method       string
	ReadinessURL string
	Insecure     bool
}

// LoadInfo describes the traffic that was generated.
type LoadInfo struct {
	Calibrated      bool
	RPS             float64
	TargetInFlight  int
	BaselineLatency time.Duration
	GoalEvaluated   bool
	Achievable      bool
	Warnings        []string
}

// Input is everything Analyze needs. Passing it all in keeps the package pure;
// see docs/adr/0011-pure-analysis-core.md.
type Input struct {
	Timeline    timeline.Timeline
	Policy      Policy
	Target      TargetInfo
	Probe       ProbeInfo
	Load        LoadInfo
	ToolVersion string
}

// Result is the analysis outcome: the public report plus the intermediate
// values, which tests and the renderer both want.
type Result struct {
	Report   schema.Report
	Facts    Facts
	Findings []Finding
	Score    ScoreResult
}

// Analyze turns a recorded run into a verdict.
//
// It is a pure function of its input: no clock, no I/O, no globals. That is
// what makes every verdict reproducible from the evidence alone, and lets a
// recorded run be re-judged offline under a different profile.
func Analyze(in Input) Result {
	facts := BuildFacts(in.Timeline, in.Policy)
	facts.TargetKind = in.Target.Kind

	var findings []Finding
	unachievable := in.Load.GoalEvaluated && !in.Load.Achievable
	if unachievable {
		expected := in.Load.RPS * in.Load.BaselineLatency.Seconds()
		findings = append(findings, Finding{
			ID: SC000, Severity: in.Policy.SeverityOf(SC000),
			Summary: "The configured in-flight goal was not achievable within the request-rate or concurrency limits.",
			Evidence: map[string]any{
				"target_in_flight":   in.Load.TargetInFlight,
				"expected_in_flight": round2(expected),
			},
		})
	}
	for _, signature := range Signatures() {
		if unachievable && signature.ID() == SC000 {
			continue
		}
		if finding, fired := signature.Evaluate(facts, in.Policy); fired {
			findings = append(findings, finding)
		}
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].ID < findings[j].ID })

	score := Score(facts, findings)
	gates := evaluateGates(facts, in.Policy, score.Value)
	verdict := decideVerdict(findings, gates)

	return Result{
		Report:   buildReport(in, facts, findings, gates, score, verdict),
		Facts:    facts,
		Findings: findings,
		Score:    score,
	}
}

// decideVerdict applies the rules in spec section 9.1.
//
// Inconclusive takes precedence over everything: if the experiment was not
// valid, no conclusion drawn from it is either, and reporting a pass would
// manufacture confidence in a service that may drop traffic on every deploy.
func decideVerdict(findings []Finding, gates []schema.Gate) schema.Verdict {
	for _, finding := range findings {
		if finding.ID == SC000 {
			return schema.VerdictInconclusive
		}
	}

	for _, finding := range findings {
		if finding.Severity == schema.SeverityError {
			return schema.VerdictFail
		}
	}
	for _, gate := range gates {
		if !gate.Passed {
			return schema.VerdictFail
		}
	}
	return schema.VerdictPass
}

func evaluateGates(f Facts, p Policy, score int) []schema.Gate {
	gates := []schema.Gate{{
		Name:      "max_inflight_drop_pct",
		Threshold: p.MaxInFlightDropPct,
		Actual:    round2(f.InFlightDropRatio() * 100),
	}}
	gates[0].Passed = gates[0].Actual <= gates[0].Threshold

	if p.MaxShutdownTime != nil {
		actual := float64(0)
		if f.ShutdownDuration != nil {
			actual = float64(f.ShutdownDuration.Milliseconds())
		}
		gates = append(gates, schema.Gate{
			Name:      "max_shutdown_time_ms",
			Threshold: float64(p.MaxShutdownTime.Milliseconds()),
			Actual:    actual,
			// A process that never exited cannot have met a shutdown budget.
			Passed: f.ShutdownDuration != nil && actual <= float64(p.MaxShutdownTime.Milliseconds()),
		})
	}

	if p.MinScore != nil {
		gates = append(gates, schema.Gate{
			Name:      "min_score",
			Threshold: float64(*p.MinScore),
			Actual:    float64(score),
			Passed:    score >= *p.MinScore,
		})
	}
	return gates
}

func buildReport(
	in Input,
	facts Facts,
	findings []Finding,
	gates []schema.Gate,
	score ScoreResult,
	verdict schema.Verdict,
) schema.Report {
	meta := in.Timeline.Meta

	report := schema.Report{
		SchemaVersion: schema.SchemaVersion,
		ToolVersion:   in.ToolVersion,
		Verdict:       verdict,
		Score:         schema.Score{Value: score.Value, Grade: score.Grade, WeightsVersion: schema.WeightsVersion},
		Run: schema.Run{
			StartedAt: meta.StartedAt,
			Profile:   string(in.Policy.Profile),
			Seed:      meta.Seed,
			Trials:    schema.Trials{Total: 1, Consistent: true},
		},
		Target: schema.Target{
			Kind:          string(in.Target.Kind),
			Label:         in.Target.Label,
			PID:           in.Target.PID,
			DetectedStack: in.Target.DetectedStack,
			GracePeriodMS: in.Policy.GracePeriod.Milliseconds(),
		},
		Probe: schema.Probe{
			URL:          in.Probe.URL,
			Method:       in.Probe.Method,
			ReadinessURL: in.Probe.ReadinessURL,
			Insecure:     in.Probe.Insecure,
		},
		Load: schema.Load{
			Calibrated:               in.Load.Calibrated,
			RPS:                      in.Load.RPS,
			TargetInFlight:           in.Load.TargetInFlight,
			ObservedInFlightAtSignal: facts.Stat(PhaseInFlight).Count,
			BaselineLatencyMS:        toLatency(facts.BaselineLatency),
			Warnings:                 in.Load.Warnings,
		},
		Timeline:    buildTimelineSection(facts),
		Requests:    buildRequests(facts),
		Connections: buildConnections(facts),
		Process:     buildProcess(facts),
		Findings:    buildFindings(findings),
		Gates:       gates,
	}

	if verdict == schema.VerdictFail {
		report.Run.Trials.Failed = 1
	}
	return report
}

func buildTimelineSection(f Facts) schema.Timeline {
	section := schema.Timeline{}
	if !f.HasSignal {
		return section
	}

	// Every instant is relative to signal delivery, so the signal itself is
	// always zero and the rest read as "how long after".
	relative := func(at *time.Duration) *int64 {
		if at == nil {
			return nil
		}
		ms := (*at - f.SignalAt).Milliseconds()
		return &ms
	}

	section.ReadinessFlippedAtMS = relative(f.ReadinessFlippedAt)
	section.ListenerClosedAtMS = relative(f.ListenerClosedAt)
	section.SigkillSentAtMS = relative(f.KillAt)
	section.ProcessExitedAtMS = relative(f.ExitAt)

	if f.ShutdownDuration != nil {
		ms := f.ShutdownDuration.Milliseconds()
		section.ShutdownDurationMS = &ms
	}
	if f.ExitAt != nil && !f.AcceptedAfterExit {
		ms := (*f.ExitAt - f.SignalAt).Milliseconds()
		section.PortReleasedAtMS = &ms
	}
	return section
}

func buildRequests(f Facts) schema.Requests {
	return schema.Requests{
		Total: len(f.Requests),
		ByPhase: schema.Phases{
			Warmup:     toPhaseStats(f.Stat(PhaseWarmup)),
			Steady:     toPhaseStats(f.Stat(PhaseSteady)),
			InFlight:   toPhaseStats(f.Stat(PhaseInFlight)),
			PostSignal: toPhaseStats(f.Stat(PhasePostSignal)),
			PostWindow: toPhaseStats(f.Stat(PhasePostWindow)),
			AtSigkill:  schema.PhaseStats{Count: f.AtSigkill},
		},
		DrainLatencyMS: toLatency(f.DrainLatency),
		Dropped:        f.DroppedRequestRecords,
	}
}

func buildConnections(f Facts) schema.Connections {
	return schema.Connections{
		Opened:                    f.Connections.Opened,
		ReusedAfterSignal:         f.Connections.ReusedAfterSignal,
		ClosedWithConnectionClose: f.Connections.ClosedWithConnectionClose,
		ClosedByFIN:               f.Connections.ClosedByFIN,
		ClosedByRST:               f.Connections.ClosedByRST,
	}
}

func buildProcess(f Facts) schema.Process {
	return schema.Process{
		ExitCode:           f.ExitCode,
		TerminatedBySignal: f.ExitSignal,
		PortHeldAfterExit:  f.AcceptedAfterExit,
	}
}

func buildFindings(findings []Finding) []schema.Finding {
	out := make([]schema.Finding, 0, len(findings))

	for _, finding := range findings {
		info, _ := Lookup(finding.ID)
		out = append(out, schema.Finding{
			ID:            string(finding.ID),
			Name:          info.Name,
			Stage:         string(info.Stage),
			Severity:      finding.Severity,
			Summary:       finding.Summary,
			Evidence:      finding.Evidence,
			Impact:        info.Impact,
			RemediationID: string(finding.ID),
			DocsURL:       "https://shutdowncheck.dev/signatures/" + string(finding.ID),
		})
	}
	return out
}

func toPhaseStats(s PhaseStats) schema.PhaseStats {
	return schema.PhaseStats{Count: s.Count, OK: s.OK, Failed: s.Failed, Failures: s.Failures}
}

func toLatency(p Percentiles) schema.Latency {
	return schema.Latency{
		P50: msFloat(p.P50),
		P95: msFloat(p.P95),
		P99: msFloat(p.P99),
	}
}

func msFloat(d time.Duration) float64 {
	return round2(float64(d) / float64(time.Millisecond))
}
