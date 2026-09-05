package analyze

import (
	"math"
	"testing"
	"time"

	"github.com/shutdowncheck/shutdowncheck/pkg/schema"
)

func TestCatalogIsComplete(t *testing.T) {
	got := Catalog()
	if len(got) != 18 {
		t.Fatalf("catalogue has %d signatures, want 18 (SC000-SC017)", len(got))
	}

	for i, info := range got {
		if want := SignatureID(idFor(i)); info.ID != want {
			t.Errorf("position %d has ID %q, want %q", i, info.ID, want)
		}
		if info.Name == "" || info.Summary == "" || info.Impact == "" {
			t.Errorf("%s is missing name, summary or impact; every signature must explain itself", info.ID)
		}
	}
}

func idFor(i int) string {
	const digits = "0123456789"
	return "SC0" + string(digits[i/10]) + string(digits[i%10])
}

func TestCatalogOrderIsStable(t *testing.T) {
	first := Catalog()
	for range 20 {
		next := Catalog()
		for i := range first {
			if next[i].ID != first[i].ID {
				t.Fatal("Catalog() order varies between calls; reports would not be reproducible")
			}
		}
	}
}

func TestLookup(t *testing.T) {
	info, ok := Lookup(SC006)
	if !ok {
		t.Fatal("SC006 is missing from the catalogue")
	}
	if info.Name != "NO_DEREGISTRATION_WINDOW" {
		t.Errorf("SC006 name = %q", info.Name)
	}
	if info.Stage != StageLameDuck {
		t.Errorf("SC006 stage = %q, want S3", info.Stage)
	}

	if _, ok := Lookup("SC999"); ok {
		t.Error("Lookup accepted an unknown identifier")
	}
}

func TestProfileValidity(t *testing.T) {
	for _, p := range Profiles() {
		if !p.Valid() {
			t.Errorf("%q is listed but reports itself invalid", p)
		}
	}
	if Profile("nonsense").Valid() {
		t.Error("an unknown profile reported itself valid")
	}
}

func TestProfileResolveFromTargetKind(t *testing.T) {
	cases := map[TargetKind]Profile{
		TargetProcess:    ProfileStandalone,
		TargetCommand:    ProfileStandalone,
		TargetDocker:     ProfileDocker,
		TargetKubernetes: ProfileKubernetes,
	}
	for kind, want := range cases {
		if got := ProfileAuto.Resolve(kind); got != want {
			t.Errorf("auto profile for %q = %q, want %q", kind, got, want)
		}
	}

	if got := ProfileStrict.Resolve(TargetKubernetes); got != ProfileStrict {
		t.Errorf("an explicit profile must not be overridden, got %q", got)
	}
}

func TestDefaultGracePeriodFollowsPlatform(t *testing.T) {
	if got := DefaultGracePeriod(TargetDocker); got != 10*time.Second {
		t.Errorf("docker grace period = %v, want 10s", got)
	}
	for _, kind := range []TargetKind{TargetProcess, TargetCommand, TargetKubernetes} {
		if got := DefaultGracePeriod(kind); got != 30*time.Second {
			t.Errorf("%q grace period = %v, want 30s", kind, got)
		}
	}
}

func TestPolicyForRejectsUnresolvedAuto(t *testing.T) {
	if _, err := PolicyFor(ProfileAuto); err == nil {
		t.Fatal("PolicyFor(auto) must fail; a report has to name a concrete profile")
	}
}

func TestPolicyForRejectsUnknownProfile(t *testing.T) {
	if _, err := PolicyFor("nonsense"); err == nil {
		t.Fatal("PolicyFor accepted an unknown profile")
	}
}

// The lame-duck family must require the service to keep serving during the
// window. Getting this backwards is exactly the mistake the tool exists to
// catch, so it is asserted directly.
func TestLameDuckProfilesRequireAcceptDuringWindow(t *testing.T) {
	for _, p := range []Profile{ProfileLameDuck, ProfileKubernetes} {
		policy, err := PolicyFor(p)
		if err != nil {
			t.Fatalf("PolicyFor(%q): %v", p, err)
		}
		if !policy.RequireAcceptDuringWindow {
			t.Errorf("%q must require connections to be served during the de-registration window", p)
		}
		if policy.AcceptWindow <= 0 {
			t.Errorf("%q has accept window %v, want a positive window", p, policy.AcceptWindow)
		}
		if got := policy.SeverityOf(SC006); got != schema.SeverityError {
			t.Errorf("%q severity for SC006 = %q, want error", p, got)
		}
		if got := policy.SeverityOf(SC007); got != schema.SeverityError {
			t.Errorf("%q severity for SC007 = %q, want error", p, got)
		}
	}
}

// Under a strict profile the opposite is true: closing immediately is correct,
// so SC006 must not be reported as a defect.
func TestStrictProfileDoesNotPenaliseImmediateClose(t *testing.T) {
	policy, err := PolicyFor(ProfileStrict)
	if err != nil {
		t.Fatalf("PolicyFor(strict): %v", err)
	}
	if policy.AcceptWindow != 0 {
		t.Errorf("strict accept window = %v, want 0", policy.AcceptWindow)
	}
	if policy.RequireAcceptDuringWindow {
		t.Error("strict must not require connections to be accepted after the signal")
	}
	if got := policy.SeverityOf(SC006); got == schema.SeverityError {
		t.Error("strict must not treat an immediate listener close as an error")
	}
}

func TestStandaloneProfileWarnsRatherThanFails(t *testing.T) {
	policy, err := PolicyFor(ProfileStandalone)
	if err != nil {
		t.Fatalf("PolicyFor(standalone): %v", err)
	}
	for _, id := range []SignatureID{SC006, SC007} {
		if got := policy.SeverityOf(id); got != schema.SeverityWarn {
			t.Errorf("standalone severity for %s = %q, want warn", id, got)
		}
	}
}

func TestDockerProfileUsesDockerGracePeriod(t *testing.T) {
	policy, err := PolicyFor(ProfileDocker)
	if err != nil {
		t.Fatalf("PolicyFor(docker): %v", err)
	}
	if policy.GracePeriod != 10*time.Second {
		t.Errorf("docker grace period = %v, want 10s", policy.GracePeriod)
	}
}

func TestSeverityOfFallsBackToDefault(t *testing.T) {
	policy, err := PolicyFor(ProfileStandalone)
	if err != nil {
		t.Fatalf("PolicyFor: %v", err)
	}
	if got := policy.SeverityOf(SC003); got != schema.SeverityError {
		t.Errorf("SC003 severity = %q, want error by default", got)
	}
	if got := policy.SeverityOf(SC009); got != schema.SeverityWarn {
		t.Errorf("SC009 severity = %q, want warn by default", got)
	}
}

func TestWithSeverityDoesNotMutateOriginal(t *testing.T) {
	base, err := PolicyFor(ProfileStandalone)
	if err != nil {
		t.Fatalf("PolicyFor: %v", err)
	}

	derived := base.WithSeverity(SC009, schema.SeverityError)

	if got := derived.SeverityOf(SC009); got != schema.SeverityError {
		t.Errorf("derived SC009 = %q, want error", got)
	}
	if got := base.SeverityOf(SC009); got != schema.SeverityWarn {
		t.Errorf("base SC009 = %q, want warn; WithSeverity must not mutate the receiver", got)
	}
}

func TestSeveritiesAreOrdered(t *testing.T) {
	policy, err := PolicyFor(ProfileKubernetes)
	if err != nil {
		t.Fatalf("PolicyFor: %v", err)
	}
	policy = policy.WithSeverity(SC014, schema.SeverityError).
		WithSeverity(SC001, schema.SeverityWarn)

	got := policy.Severities()
	for i := 1; i < len(got); i++ {
		if got[i].ID < got[i-1].ID {
			t.Fatalf("Severities() is not ordered: %v", got)
		}
	}
}

func TestPolicyValidate(t *testing.T) {
	valid := func(t *testing.T) Policy {
		t.Helper()
		p, err := PolicyFor(ProfileKubernetes)
		if err != nil {
			t.Fatalf("PolicyFor: %v", err)
		}
		return p
	}

	t.Run("default is valid", func(t *testing.T) {
		if err := valid(t).Validate(); err != nil {
			t.Fatalf("default kubernetes policy is invalid: %v", err)
		}
	})

	t.Run("accept window must be shorter than grace", func(t *testing.T) {
		p := valid(t)
		p.AcceptWindow = p.GracePeriod
		if err := p.Validate(); err == nil {
			t.Fatal("expected an error when the accept window is not shorter than the grace period")
		}
	})

	t.Run("lame-duck needs a non-zero window", func(t *testing.T) {
		p := valid(t)
		p.AcceptWindow = 0
		if err := p.Validate(); err == nil {
			t.Fatal("expected an error when a lame-duck profile has a zero accept window")
		}
	})

	t.Run("rejects bad numbers", func(t *testing.T) {
		cases := map[string]func(*Policy){
			"zero grace":                func(p *Policy) { p.GracePeriod = 0 },
			"negative window":           func(p *Policy) { p.AcceptWindow = -time.Second },
			"negative dereg min":        func(p *Policy) { p.DeregMin = -time.Second },
			"negative readiness budget": func(p *Policy) { p.ReadinessFlipBudget = -time.Second },
			"excessive grace":           func(p *Policy) { p.GracePeriod = MaxPolicyDuration + time.Second },
			"zero sample":               func(p *Policy) { p.MinInFlightSample = 0 },
			"drop pct over100":          func(p *Policy) { p.MaxInFlightDropPct = 101 },
			"drop pct NaN":              func(p *Policy) { p.MaxInFlightDropPct = math.NaN() },
			"latency factor Inf":        func(p *Policy) { p.LatencySpikeFactor = math.Inf(1) },
			"negative budget":           func(p *Policy) { d := -time.Second; p.MaxShutdownTime = &d },
			"score over 100":            func(p *Policy) { s := 101; p.MinScore = &s },
		}
		for name, mutate := range cases {
			t.Run(name, func(t *testing.T) {
				p := valid(t)
				mutate(&p)
				if err := p.Validate(); err == nil {
					t.Fatalf("expected an error for %s", name)
				}
			})
		}
	})

	t.Run("unresolved profile is invalid", func(t *testing.T) {
		p := valid(t)
		p.Profile = ProfileAuto
		if err := p.Validate(); err == nil {
			t.Fatal("an unresolved auto profile must not validate")
		}
	})
}

func TestDefaultSeverityCoversEverySignature(t *testing.T) {
	for _, info := range Catalog() {
		switch DefaultSeverity(info.ID) {
		case schema.SeverityError, schema.SeverityWarn, schema.SeverityInfo:
		default:
			t.Errorf("%s has no valid default severity", info.ID)
		}
	}
}
