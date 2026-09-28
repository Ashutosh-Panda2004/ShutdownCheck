package cli

import (
	"fmt"
	"math"

	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/analyze"
	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/config"
	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/redact"
	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/timeline"
	"github.com/Ashutosh-Panda2004/ShutdownCheck/pkg/schema"
)

const analysisContextVersion = 1

func encodeAnalysisContext(input analyze.Input) *timeline.AnalysisContext {
	policy := input.Policy
	encoded := &timeline.AnalysisContext{
		Version: analysisContextVersion,
		Target: timeline.AnalysisTarget{
			Kind: string(input.Target.Kind), Label: input.Target.Label,
			PID: input.Target.PID, DetectedStack: input.Target.DetectedStack,
		},
		Probe: timeline.AnalysisProbe{
			URL: input.Probe.URL, Method: input.Probe.Method,
			ReadinessURL: input.Probe.ReadinessURL, Insecure: input.Probe.Insecure,
		},
		Load: timeline.AnalysisLoad{
			Calibrated: input.Load.Calibrated, RPS: input.Load.RPS,
			TargetInFlight: input.Load.TargetInFlight, BaselineLatency: input.Load.BaselineLatency,
			GoalEvaluated: input.Load.GoalEvaluated,
			Achievable:    input.Load.Achievable, Warnings: append([]string(nil), input.Load.Warnings...),
		},
		Policy: timeline.AnalysisPolicy{
			Profile: string(policy.Profile), AcceptWindow: policy.AcceptWindow,
			RequireAcceptDuringWindow: policy.RequireAcceptDuringWindow,
			DeregMin:                  policy.DeregMin, ReadinessFlipBudget: policy.ReadinessFlipBudget,
			RequireReadinessFlip: policy.RequireReadinessFlip, GracePeriod: policy.GracePeriod,
			MinInFlightSample: policy.MinInFlightSample, MaxInFlightDropPct: policy.MaxInFlightDropPct,
			LatencySpikeFactor: policy.LatencySpikeFactor,
		},
	}
	if policy.MaxShutdownTime != nil {
		value := *policy.MaxShutdownTime
		encoded.Policy.MaxShutdownTime = &value
	}
	if policy.MinScore != nil {
		value := *policy.MinScore
		encoded.Policy.MinScore = &value
	}
	for _, override := range policy.Severities() {
		encoded.Policy.Severities = append(encoded.Policy.Severities, timeline.AnalysisSeverity{
			ID: string(override.ID), Severity: string(override.Severity),
		})
	}
	return encoded
}

func decodeAnalysisContext(tl timeline.Timeline) (analyze.Input, timeline.AnalysisTrials, error) {
	context := tl.Meta.Analysis
	if context == nil {
		return analyze.Input{}, timeline.AnalysisTrials{}, fmt.Errorf("recording has no analysis context")
	}
	if context.Version != analysisContextVersion {
		return analyze.Input{}, timeline.AnalysisTrials{}, fmt.Errorf("unsupported analysis context version %d", context.Version)
	}

	kind := analyze.TargetKind(context.Target.Kind)
	switch kind {
	case analyze.TargetProcess, analyze.TargetCommand, analyze.TargetDocker, analyze.TargetKubernetes:
	default:
		return analyze.Input{}, timeline.AnalysisTrials{}, fmt.Errorf("recording has unknown target kind %q", kind)
	}
	if math.IsNaN(context.Load.RPS) || math.IsInf(context.Load.RPS, 0) ||
		context.Load.RPS < 0 || context.Load.RPS > config.MaxRPS {
		return analyze.Input{}, timeline.AnalysisTrials{}, fmt.Errorf("recording has invalid request rate %v", context.Load.RPS)
	}
	if context.Load.TargetInFlight < 0 || context.Load.BaselineLatency < 0 ||
		context.Load.BaselineLatency > config.MaxOperationalDuration {
		return analyze.Input{}, timeline.AnalysisTrials{}, fmt.Errorf("recording has invalid load metadata")
	}
	if context.Target.PID < 0 {
		return analyze.Input{}, timeline.AnalysisTrials{}, fmt.Errorf("recording has invalid target PID")
	}

	stored := context.Policy
	policy := analyze.Policy{
		Profile: analyze.Profile(stored.Profile), AcceptWindow: stored.AcceptWindow,
		RequireAcceptDuringWindow: stored.RequireAcceptDuringWindow,
		DeregMin:                  stored.DeregMin, ReadinessFlipBudget: stored.ReadinessFlipBudget,
		RequireReadinessFlip: stored.RequireReadinessFlip, GracePeriod: stored.GracePeriod,
		MaxShutdownTime: stored.MaxShutdownTime, MinInFlightSample: stored.MinInFlightSample,
		MaxInFlightDropPct: stored.MaxInFlightDropPct, MinScore: stored.MinScore,
		LatencySpikeFactor: stored.LatencySpikeFactor,
	}
	seen := map[analyze.SignatureID]bool{}
	for _, override := range stored.Severities {
		id := analyze.SignatureID(override.ID)
		if _, ok := analyze.Lookup(id); !ok {
			return analyze.Input{}, timeline.AnalysisTrials{}, fmt.Errorf("recording has unknown signature override %q", id)
		}
		if seen[id] {
			return analyze.Input{}, timeline.AnalysisTrials{}, fmt.Errorf("recording repeats signature override %q", id)
		}
		seen[id] = true
		severity := schema.Severity(override.Severity)
		if severity != schema.SeverityError && severity != schema.SeverityWarn && severity != schema.SeverityInfo {
			return analyze.Input{}, timeline.AnalysisTrials{}, fmt.Errorf("recording has invalid severity %q", severity)
		}
		policy = policy.WithSeverity(id, severity)
	}
	if err := policy.Validate(); err != nil {
		return analyze.Input{}, timeline.AnalysisTrials{}, fmt.Errorf("recording has invalid policy: %w", err)
	}
	if context.Trials.Total < 1 || context.Trials.Total > config.MaxTrials ||
		context.Trials.Failed < 0 || context.Trials.Failed > context.Trials.Total {
		return analyze.Input{}, timeline.AnalysisTrials{}, fmt.Errorf("recording has invalid trial summary")
	}

	return analyze.Input{
		Timeline: tl,
		Target: analyze.TargetInfo{
			Kind: kind, Label: redact.Text(context.Target.Label), PID: context.Target.PID,
			DetectedStack: redact.Text(context.Target.DetectedStack),
		},
		Probe: analyze.ProbeInfo{
			URL: redact.URL(context.Probe.URL), Method: redact.Text(context.Probe.Method),
			ReadinessURL: redact.URL(context.Probe.ReadinessURL), Insecure: context.Probe.Insecure,
		},
		Load: analyze.LoadInfo{
			Calibrated: context.Load.Calibrated, RPS: context.Load.RPS,
			TargetInFlight: context.Load.TargetInFlight, BaselineLatency: context.Load.BaselineLatency,
			GoalEvaluated: context.Load.GoalEvaluated,
			Achievable:    context.Load.Achievable, Warnings: sanitizedStrings(context.Load.Warnings),
		},
		Policy: policy,
	}, context.Trials, nil
}

func sanitizedStrings(values []string) []string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = redact.Message(value)
	}
	return out
}
