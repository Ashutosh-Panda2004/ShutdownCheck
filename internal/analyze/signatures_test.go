package analyze

import (
	"testing"
	"time"

	"github.com/shutdowncheck/shutdowncheck/internal/timeline"
)

// fixture builds synthetic timelines.
//
// Because analysis is pure, every rule can be exercised against a hand-built
// timeline with no network, no subprocess and no waiting. That is what makes a
// positive and a negative case per signature realistic rather than aspirational.
type fixture struct {
	events []timeline.Event
}

func newFixture() *fixture { return &fixture{} }

func (f *fixture) add(e timeline.Event) *fixture {
	f.events = append(f.events, e)
	return f
}

func (f *fixture) signal(at time.Duration) *fixture {
	return f.add(timeline.SignalAt(at, timeline.SignalEvent{Signal: "TERM"}))
}

func (f *fixture) kill(at time.Duration) *fixture {
	return f.add(timeline.SignalAt(at, timeline.SignalEvent{Signal: "KILL"}))
}

func (f *fixture) exit(at time.Duration, code int) *fixture {
	c := code
	return f.add(timeline.ProcessAt(at, timeline.ProcessEvent{Phase: timeline.ProcExited, PID: 42, ExitCode: &c}))
}

func (f *fixture) exitBySignal(at time.Duration, by string) *fixture {
	return f.add(timeline.ProcessAt(at, timeline.ProcessEvent{Phase: timeline.ProcExited, PID: 42, TerminatedBy: by}))
}

func (f *fixture) requests(n int, sent, done time.Duration, outcome timeline.Outcome) *fixture {
	for range n {
		f.add(timeline.RequestAt(timeline.RequestEvent{
			Method: "GET", URL: "http://x/", Sent: sent, Done: done, Outcome: outcome,
		}))
	}
	return f
}

func (f *fixture) warmup(n int, sent, done time.Duration) *fixture {
	for range n {
		f.add(timeline.RequestAt(timeline.RequestEvent{
			Method: "GET", URL: "http://x/", Sent: sent, Done: done,
			Outcome: timeline.OutcomeOK, Warmup: true,
		}))
	}
	return f
}

func (f *fixture) readiness(at time.Duration, healthy bool) *fixture {
	status := 200
	if !healthy {
		status = 503
	}
	return f.add(timeline.ReadinessAt(at, timeline.ReadinessEvent{Status: status, Healthy: healthy}))
}

func (f *fixture) listener(at time.Duration, accepting bool) *fixture {
	outcome := timeline.OutcomeOK
	if !accepting {
		outcome = timeline.OutcomeRefused
	}
	return f.add(timeline.ListenerAt(at, timeline.ListenerEvent{Accepting: accepting, Outcome: outcome}))
}

func (f *fixture) connOpen(at time.Duration, id uint64) *fixture {
	return f.add(timeline.ConnectionAt(at, timeline.ConnectionEvent{ID: id, Phase: timeline.ConnOpen}))
}

func (f *fixture) connReuse(at time.Duration, id uint64) *fixture {
	return f.add(timeline.ConnectionAt(at, timeline.ConnectionEvent{ID: id, Phase: timeline.ConnReuse, Reused: true}))
}

func (f *fixture) connClose(at time.Duration, id uint64, term timeline.ConnTermination, serverClose bool) *fixture {
	return f.add(timeline.ConnectionAt(at, timeline.ConnectionEvent{
		ID: id, Phase: timeline.ConnClose, Termination: term, ServerClose: serverClose,
	}))
}

func (f *fixture) build() timeline.Timeline {
	rec := timeline.NewRecorder(timeline.Meta{ToolVersion: "test", Trial: 1, Trials: 1}, 0)
	for _, e := range f.events {
		rec.Record(e)
	}
	return rec.Snapshot()
}

// Timings shared by every fixture. The signal sits at 5s so there is room for a
// realistic steady period before it.
const (
	fxSignal      = 5 * time.Second
	fxWindowEnd   = 10 * time.Second // signal + 5s accept window
	fxListenClose = 10500 * time.Millisecond
	fxExit        = 12 * time.Second
)

func lameDuckPolicy(t *testing.T) Policy {
	t.Helper()

	policy, err := PolicyFor(ProfileLameDuck)
	if err != nil {
		t.Fatalf("PolicyFor: %v", err)
	}
	return policy
}

// healthyRun is a service that does everything right: it flips readiness
// immediately, keeps serving through the de-registration window, closes the
// listener afterwards, finishes its in-flight work, and exits cleanly.
func healthyRun() *fixture {
	f := newFixture()

	f.warmup(5, 500*time.Millisecond, 600*time.Millisecond)
	f.requests(10, time.Second, 1100*time.Millisecond, timeline.OutcomeOK)           // steady
	f.requests(10, 4900*time.Millisecond, 5100*time.Millisecond, timeline.OutcomeOK) // in flight
	f.requests(5, 6*time.Second, 6100*time.Millisecond, timeline.OutcomeOK)          // inside the window
	f.requests(5, 11*time.Second, 11050*time.Millisecond, timeline.OutcomeRefused)   // after it closed

	for at := time.Second; at < fxSignal; at += time.Second {
		f.readiness(at, true)
		f.listener(at, true)
	}
	f.readiness(5200*time.Millisecond, false)
	for at := 6 * time.Second; at <= 9*time.Second; at += time.Second {
		f.listener(at, true)
		f.readiness(at, false)
	}
	f.listener(fxListenClose, false)
	f.readiness(11*time.Second, false)

	f.connOpen(time.Second, 1)
	f.connReuse(6*time.Second, 1)
	f.connClose(11*time.Second, 1, timeline.TermFIN, true)

	f.signal(fxSignal)
	f.exit(fxExit, 0)
	return f
}

type signatureCase struct {
	// positive must make the signature fire; negative must not.
	positive func() *fixture
	negative func() *fixture
	// policy overrides the default lame-duck policy when a rule needs one.
	policy func(t *testing.T) Policy
}

var signatureCases = map[SignatureID]signatureCase{
	SC000: {
		positive: func() *fixture {
			f := healthyRun()
			f.events = nil
			return f.signal(fxSignal).exit(fxExit, 0)
		},
		negative: healthyRun,
	},
	SC001: {
		positive: func() *fixture {
			// Nothing reacts: readiness stays healthy, listener stays open, no exit.
			f := newFixture()
			f.requests(10, 4900*time.Millisecond, 5100*time.Millisecond, timeline.OutcomeOK)
			for at := time.Second; at <= 20*time.Second; at += time.Second {
				f.readiness(at, true)
				f.listener(at, true)
			}
			return f.signal(fxSignal)
		},
		negative: healthyRun,
	},
	SC002: {
		positive: func() *fixture {
			f := healthyRun()
			return f.kill(35 * time.Second)
		},
		negative: healthyRun,
	},
	SC003: {
		positive: func() *fixture {
			f := newFixture()
			f.requests(6, 4900*time.Millisecond, 5100*time.Millisecond, timeline.OutcomeOK)
			f.requests(4, 4900*time.Millisecond, 5050*time.Millisecond, timeline.OutcomeReset)
			return f.signal(fxSignal).exit(fxExit, 0)
		},
		negative: healthyRun,
	},
	SC004: {
		positive: func() *fixture {
			f := healthyRun()
			return f.connClose(6*time.Second, 2, timeline.TermRST, false)
		},
		negative: healthyRun,
	},
	SC005: {
		positive: func() *fixture {
			f := healthyRun()
			return f.listener(11*time.Second, true)
		},
		negative: healthyRun,
	},
	SC006: {
		positive: func() *fixture {
			f := newFixture()
			f.requests(10, 4900*time.Millisecond, 5100*time.Millisecond, timeline.OutcomeOK)
			f.listener(4*time.Second, true)
			f.listener(5010*time.Millisecond, false) // closed 10ms after the signal
			return f.signal(fxSignal).exit(fxExit, 0)
		},
		negative: healthyRun,
	},
	SC007: {
		positive: func() *fixture {
			f := newFixture()
			f.requests(10, 4900*time.Millisecond, 5100*time.Millisecond, timeline.OutcomeOK)
			for at := time.Second; at <= 11*time.Second; at += time.Second {
				f.readiness(at, true)
			}
			return f.signal(fxSignal).exit(fxExit, 0)
		},
		negative: healthyRun,
	},
	SC008: {
		positive: func() *fixture {
			f := newFixture()
			f.requests(10, 4900*time.Millisecond, 5100*time.Millisecond, timeline.OutcomeOK)
			f.readiness(time.Second, true)
			f.readiness(8*time.Second, false) // 3s after the signal
			return f.signal(fxSignal).exit(fxExit, 0)
		},
		negative: healthyRun,
	},
	SC009: {
		positive: func() *fixture {
			f := newFixture()
			f.requests(10, 4900*time.Millisecond, 5100*time.Millisecond, timeline.OutcomeOK)
			f.connOpen(time.Second, 1)
			f.connReuse(6*time.Second, 1)
			f.connClose(11*time.Second, 1, timeline.TermFIN, false) // never asked the client to close
			return f.signal(fxSignal).exit(fxExit, 0)
		},
		negative: healthyRun,
	},
	SC010: {
		positive: healthyRun, // drains in 7s
		negative: func() *fixture {
			f := newFixture()
			f.requests(10, 4900*time.Millisecond, 5100*time.Millisecond, timeline.OutcomeOK)
			return f.signal(fxSignal).exit(5400*time.Millisecond, 0) // well inside the budget
		},
		policy: func(t *testing.T) Policy {
			t.Helper()
			p := lameDuckPolicy(t)
			budget := time.Second
			p.MaxShutdownTime = &budget
			return p
		},
	},
	SC011: {
		positive: func() *fixture {
			f := newFixture()
			f.requests(10, 4900*time.Millisecond, 5100*time.Millisecond, timeline.OutcomeOK)
			// Still being processed when the process walked out.
			f.requests(3, 6*time.Second, 9*time.Second, timeline.OutcomeReset)
			return f.signal(fxSignal).exit(8*time.Second, 0)
		},
		negative: healthyRun,
	},
	SC012: {
		positive: func() *fixture {
			f := healthyRun()
			return f.listener(13*time.Second, true) // after exit at 12s
		},
		negative: healthyRun,
	},
	SC013: {
		positive: func() *fixture {
			f := newFixture()
			f.requests(10, 4900*time.Millisecond, 5100*time.Millisecond, timeline.OutcomeOK)
			return f.signal(fxSignal).exit(fxExit, 3)
		},
		negative: healthyRun,
	},
	SC014: {
		positive: func() *fixture {
			f := newFixture()
			f.requests(10, time.Second, 1010*time.Millisecond, timeline.OutcomeOK) // 10ms baseline
			f.requests(10, 4900*time.Millisecond, 6*time.Second, timeline.OutcomeOK)
			return f.signal(fxSignal).exit(fxExit, 0)
		},
		negative: healthyRun,
	},
	SC015: {
		positive: func() *fixture {
			f := healthyRun()
			return f.requests(3, 6500*time.Millisecond, 9*time.Second, timeline.OutcomeTimeout)
		},
		negative: healthyRun,
	},
	SC016: {
		positive: func() *fixture {
			f := newFixture()
			f.requests(10, 4900*time.Millisecond, 5100*time.Millisecond, timeline.OutcomeOK)
			f.readiness(time.Second, true)
			f.readiness(5500*time.Millisecond, false)
			f.readiness(7*time.Second, true) // recovered while terminating
			return f.signal(fxSignal).exit(fxExit, 0)
		},
		negative: healthyRun,
	},
	SC017: {
		positive: func() *fixture {
			f := healthyRun()
			f.kill(35 * time.Second)
			return f.requests(4, 34*time.Second, 36*time.Second, timeline.OutcomeReset)
		},
		negative: healthyRun,
	},
}

// Every registered rule must have both cases. Without this guard the guarantee
// quietly rots as signatures are added, and an untested verdict rule is exactly
// the kind of liability this project cannot carry.
func TestEverySignatureHasPositiveAndNegativeFixtures(t *testing.T) {
	for _, signature := range Signatures() {
		id := signature.ID()

		tc, ok := signatureCases[id]
		if !ok {
			t.Errorf("%s has no fixtures; add a positive and a negative case", id)
			continue
		}
		if tc.positive == nil || tc.negative == nil {
			t.Errorf("%s needs both a positive and a negative fixture", id)
		}
	}

	for id := range signatureCases {
		if _, ok := SignatureByID(id); !ok {
			t.Errorf("fixtures exist for %s but it is not registered", id)
		}
	}
}

func TestSignatureFixtures(t *testing.T) {
	for _, signature := range Signatures() {
		id := signature.ID()
		tc := signatureCases[id]

		policy := lameDuckPolicy(t)
		if tc.policy != nil {
			policy = tc.policy(t)
		}

		t.Run(string(id)+"/fires", func(t *testing.T) {
			facts := BuildFacts(tc.positive().build(), policy)
			finding, fired := signature.Evaluate(facts, policy)
			if !fired {
				t.Fatalf("%s did not fire on its positive fixture", id)
			}
			if finding.Summary == "" {
				t.Errorf("%s fired without a summary; a finding must explain itself", id)
			}
			if len(finding.Evidence) == 0 {
				t.Errorf("%s fired without evidence; a verdict has to be checkable", id)
			}
			if finding.ID != id {
				t.Errorf("finding reports id %s, want %s", finding.ID, id)
			}
		})

		t.Run(string(id)+"/quiet", func(t *testing.T) {
			facts := BuildFacts(tc.negative().build(), policy)
			if finding, fired := signature.Evaluate(facts, policy); fired {
				t.Fatalf("%s fired on a correct shutdown: %s", id, finding.Summary)
			}
		})
	}
}

// The whole point of the tool is that a correct service passes cleanly.
func TestHealthyRunPasses(t *testing.T) {
	result := Analyze(Input{
		Timeline:    healthyRun().build(),
		Policy:      lameDuckPolicy(t),
		ToolVersion: "test",
	})

	if result.Report.Verdict != "pass" {
		t.Fatalf("verdict = %q, want pass. findings: %v", result.Report.Verdict, summaries(result.Findings))
	}
	if result.Score.Value != 100 {
		t.Errorf("score = %d, want 100 (deductions: %v)", result.Score.Value, result.Score.Deductions)
	}
}

func summaries(findings []Finding) []string {
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, string(f.ID)+": "+f.Summary)
	}
	return out
}
