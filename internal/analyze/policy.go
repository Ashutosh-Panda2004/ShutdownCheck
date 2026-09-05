package analyze

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/shutdowncheck/shutdowncheck/pkg/schema"
)

// Profile encodes what correct termination means for a deployment model.
//
// This is the tool's sharpest distinction. Accepting a connection 200ms after
// SIGTERM is required behaviour under Kubernetes and a defect under a strict
// standalone model, so no single threshold can judge both. See
// docs/adr/0005-shutdown-profiles.md.
type Profile string

const (
	// ProfileAuto selects a concrete profile from the target kind.
	ProfileAuto Profile = "auto"
	// ProfileStandalone makes no assumption about a load balancer.
	ProfileStandalone Profile = "standalone"
	// ProfileStrict requires the listener to close immediately.
	ProfileStrict Profile = "strict"
	// ProfileLameDuck requires the service to keep serving during the
	// de-registration window, then stop.
	ProfileLameDuck Profile = "lame-duck"
	// ProfileKubernetes is lame-duck with Kubernetes defaults.
	ProfileKubernetes Profile = "kubernetes"
	// ProfileDocker matches docker stop semantics.
	ProfileDocker Profile = "docker"
)

// Profiles lists the selectable profiles, for validation and help text.
func Profiles() []Profile {
	return []Profile{
		ProfileAuto, ProfileStandalone, ProfileStrict,
		ProfileLameDuck, ProfileKubernetes, ProfileDocker,
	}
}

// Valid reports whether p is a known profile.
func (p Profile) Valid() bool {
	for _, known := range Profiles() {
		if p == known {
			return true
		}
	}
	return false
}

// TargetKind is how the target was addressed. It selects the concrete profile
// under ProfileAuto and the default grace period.
type TargetKind string

// TargetProcess and the other target constants identify supported target
// categories.
const (
	TargetProcess    TargetKind = "process"
	TargetCommand    TargetKind = "command"
	TargetDocker     TargetKind = "docker"
	TargetKubernetes TargetKind = "kubernetes"
)

// Default grace periods follow the platform rather than a single global
// constant: Kubernetes waits 30s by default, docker stop waits 10s.
const (
	DefaultProcessGracePeriod = 30 * time.Second
	DefaultDockerGracePeriod  = 10 * time.Second
)

// DefaultGracePeriod returns the grace period a real orchestrator would apply
// to this kind of target.
func DefaultGracePeriod(kind TargetKind) time.Duration {
	if kind == TargetDocker {
		return DefaultDockerGracePeriod
	}
	return DefaultProcessGracePeriod
}

// Resolve turns ProfileAuto into a concrete profile.
func (p Profile) Resolve(kind TargetKind) Profile {
	if p != ProfileAuto {
		return p
	}
	switch kind {
	case TargetKubernetes:
		return ProfileKubernetes
	case TargetDocker:
		return ProfileDocker
	default:
		return ProfileStandalone
	}
}

// Policy is everything analysis needs in order to judge a timeline. It is data
// only: constructing it is configuration's job, and interpreting it is
// analysis's.
type Policy struct {
	Profile Profile

	// AcceptWindow is how long after the signal new connections may still be
	// accepted.
	AcceptWindow time.Duration
	// RequireAcceptDuringWindow makes refusing connections inside the window a
	// defect, which is what catches the Kubernetes 502 pattern.
	RequireAcceptDuringWindow bool
	// DeregMin is the shortest acceptable gap between the signal and listener
	// close when a de-registration window is expected.
	DeregMin time.Duration
	// ReadinessFlipBudget is how quickly readiness must start failing.
	ReadinessFlipBudget time.Duration
	// RequireReadinessFlip makes a readiness endpoint that never fails an error
	// rather than a warning.
	RequireReadinessFlip bool

	GracePeriod        time.Duration
	MaxShutdownTime    *time.Duration
	MinInFlightSample  int
	MaxInFlightDropPct float64
	MinScore           *int
	LatencySpikeFactor float64

	severities map[SignatureID]schema.Severity
}

// Baseline policy values that are not profile-specific.
const (
	DefaultMinInFlightSample  = 5
	DefaultLatencySpikeFactor = 3.0
	DefaultAcceptWindow       = 5 * time.Second
	DefaultDeregMin           = 500 * time.Millisecond
	DefaultReadinessFlip      = time.Second
	MaxPolicyDuration         = 24 * time.Hour
	// MaxSignalSkew is the delivery lateness above which phase attribution is
	// no longer trustworthy.
	MaxSignalSkew = 50 * time.Millisecond
)

// PolicyFor returns the default policy for a profile.
//
// ProfileAuto is rejected: callers must resolve it against a target kind first,
// so that a report can always state which concrete profile produced the verdict.
func PolicyFor(p Profile) (Policy, error) {
	if p == ProfileAuto {
		return Policy{}, fmt.Errorf("profile %q must be resolved against a target kind before use", p)
	}
	if !p.Valid() {
		return Policy{}, fmt.Errorf("unknown profile %q (valid: %v)", p, Profiles())
	}

	policy := Policy{
		Profile:             p,
		DeregMin:            DefaultDeregMin,
		ReadinessFlipBudget: DefaultReadinessFlip,
		GracePeriod:         DefaultProcessGracePeriod,
		MinInFlightSample:   DefaultMinInFlightSample,
		LatencySpikeFactor:  DefaultLatencySpikeFactor,
		severities:          map[SignatureID]schema.Severity{},
	}

	switch p {
	case ProfileStrict:
		policy.AcceptWindow = 0
		policy.severities[SC006] = schema.SeverityInfo // closing immediately is the requirement here

	case ProfileStandalone:
		policy.AcceptWindow = 0
		policy.severities[SC006] = schema.SeverityWarn
		policy.severities[SC007] = schema.SeverityWarn

	case ProfileDocker:
		policy.AcceptWindow = 0
		policy.GracePeriod = DefaultDockerGracePeriod
		policy.severities[SC006] = schema.SeverityWarn
		policy.severities[SC007] = schema.SeverityWarn

	case ProfileLameDuck, ProfileKubernetes:
		policy.AcceptWindow = DefaultAcceptWindow
		policy.RequireAcceptDuringWindow = true
		policy.RequireReadinessFlip = true
		policy.severities[SC006] = schema.SeverityError
		policy.severities[SC007] = schema.SeverityError
	}

	return policy, nil
}

// SeverityOf returns the effective severity of a signature under this policy.
func (p Policy) SeverityOf(id SignatureID) schema.Severity {
	if sev, ok := p.severities[id]; ok {
		return sev
	}
	return DefaultSeverity(id)
}

// WithSeverity returns a copy of the policy with one signature's severity
// overridden, implementing --fail-on and --ignore.
func (p Policy) WithSeverity(id SignatureID, sev schema.Severity) Policy {
	next := p
	next.severities = make(map[SignatureID]schema.Severity, len(p.severities)+1)
	for k, v := range p.severities {
		next.severities[k] = v
	}
	next.severities[id] = sev
	return next
}

// Severities returns every explicit override, ordered by signature ID so that
// rendering a policy is reproducible.
func (p Policy) Severities() []struct {
	ID       SignatureID
	Severity schema.Severity
} {
	out := make([]struct {
		ID       SignatureID
		Severity schema.Severity
	}, 0, len(p.severities))

	for id, sev := range p.severities {
		out = append(out, struct {
			ID       SignatureID
			Severity schema.Severity
		}{id, sev})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Validate checks the policy is internally coherent.
func (p Policy) Validate() error {
	if !p.Profile.Valid() || p.Profile == ProfileAuto {
		return fmt.Errorf("policy has unresolved or invalid profile %q", p.Profile)
	}
	if p.GracePeriod <= 0 {
		return fmt.Errorf("grace period must be positive, got %v", p.GracePeriod)
	}
	if p.AcceptWindow < 0 {
		return fmt.Errorf("accept window cannot be negative, got %v", p.AcceptWindow)
	}
	if p.DeregMin < 0 {
		return fmt.Errorf("minimum deregistration window cannot be negative, got %v", p.DeregMin)
	}
	if p.ReadinessFlipBudget < 0 {
		return fmt.Errorf("readiness flip budget cannot be negative, got %v", p.ReadinessFlipBudget)
	}
	if p.GracePeriod > MaxPolicyDuration || p.AcceptWindow > MaxPolicyDuration ||
		p.DeregMin > MaxPolicyDuration || p.ReadinessFlipBudget > MaxPolicyDuration {
		return fmt.Errorf("policy durations must not exceed %v", MaxPolicyDuration)
	}
	if p.AcceptWindow >= p.GracePeriod {
		return fmt.Errorf(
			"accept window %v must be shorter than the grace period %v, otherwise the service is required to keep accepting traffic until it is killed",
			p.AcceptWindow, p.GracePeriod)
	}
	if p.RequireAcceptDuringWindow && p.AcceptWindow == 0 {
		return fmt.Errorf("profile %q requires connections to be accepted after the signal but the accept window is zero", p.Profile)
	}
	if p.MinInFlightSample < 1 {
		return fmt.Errorf("minimum in-flight sample must be at least 1, got %d", p.MinInFlightSample)
	}
	if math.IsNaN(p.MaxInFlightDropPct) || math.IsInf(p.MaxInFlightDropPct, 0) ||
		p.MaxInFlightDropPct < 0 || p.MaxInFlightDropPct > 100 {
		return fmt.Errorf("maximum in-flight drop percentage must be between 0 and 100, got %v", p.MaxInFlightDropPct)
	}
	if p.MaxShutdownTime != nil && *p.MaxShutdownTime <= 0 {
		return fmt.Errorf("maximum shutdown time must be positive, got %v", *p.MaxShutdownTime)
	}
	if p.MaxShutdownTime != nil && *p.MaxShutdownTime > MaxPolicyDuration {
		return fmt.Errorf("maximum shutdown time must not exceed %v", MaxPolicyDuration)
	}
	if p.MinScore != nil && (*p.MinScore < 0 || *p.MinScore > 100) {
		return fmt.Errorf("minimum score must be between 0 and 100, got %d", *p.MinScore)
	}
	if math.IsNaN(p.LatencySpikeFactor) || math.IsInf(p.LatencySpikeFactor, 0) || p.LatencySpikeFactor <= 0 {
		return fmt.Errorf("latency spike factor must be finite and positive, got %v", p.LatencySpikeFactor)
	}
	return nil
}
