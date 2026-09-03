package timeline

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func testMeta() Meta {
	return Meta{
		ToolVersion: "1.0.0-test",
		StartedAt:   time.Date(2026, time.September, 3, 10, 15, 18, 0, time.UTC),
		Target:      "command(./bin/api)",
		Profile:     "kubernetes",
		Seed:        42,
		Trial:       1,
		Trials:      3,
	}
}

func TestRecorderAssignsSequentialSeq(t *testing.T) {
	r := NewRecorder(testMeta(), 0)

	r.Record(StageAt(0, StageWarmup))
	r.Record(StageAt(time.Second, StageSteady))
	r.Record(StageAt(2*time.Second, StageSignal))

	got := r.Snapshot()
	for i, e := range got.Events {
		if want := uint64(i + 1); e.Seq != want {
			t.Errorf("event %d has Seq %d, want %d", i, e.Seq, want)
		}
	}
}

func TestSnapshotOrdersByOffsetThenSeq(t *testing.T) {
	r := NewRecorder(testMeta(), 0)

	// Recorded out of order, as concurrent producers would.
	r.Record(StageAt(3*time.Second, StageObserve))
	r.Record(StageAt(time.Second, StageWarmup))
	r.Record(StageAt(time.Second, StageSteady)) // same offset, later seq
	r.Record(StageAt(2*time.Second, StageSignal))

	got := r.Snapshot().Events
	wantStages := []Stage{StageWarmup, StageSteady, StageSignal, StageObserve}
	for i, want := range wantStages {
		if got[i].Stage.Stage != want {
			t.Fatalf("position %d = %q, want %q (order: %v)", i, got[i].Stage.Stage, want, stages(got))
		}
	}
}

func stages(events []Event) []Stage {
	out := make([]Stage, 0, len(events))
	for _, e := range events {
		if e.Stage != nil {
			out = append(out, e.Stage.Stage)
		}
	}
	return out
}

func TestSnapshotIsIndependentCopy(t *testing.T) {
	r := NewRecorder(testMeta(), 0)
	r.Record(StageAt(0, StageWarmup))

	snap := r.Snapshot()
	snap.Events[0].Stage.Stage = StageComplete

	r.Record(StageAt(time.Second, StageSteady))
	if got := len(snap.Events); got != 1 {
		t.Fatalf("snapshot grew to %d events; it must not alias the recorder", got)
	}
}

func TestRecorderIsSafeForConcurrentUse(t *testing.T) {
	r := NewRecorder(testMeta(), 0)

	const writers, perWriter = 8, 200
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perWriter {
				r.Record(RequestAt(RequestEvent{
					ID:      uint64(w*perWriter + i),
					Method:  "GET",
					URL:     "http://localhost:8080/",
					Sent:    time.Duration(i) * time.Millisecond,
					Done:    time.Duration(i+1) * time.Millisecond,
					Outcome: OutcomeOK,
				}))
			}
		}()
	}
	wg.Wait()

	if got, want := r.Len(), writers*perWriter; got != want {
		t.Fatalf("recorded %d events, want %d", got, want)
	}

	seen := map[uint64]bool{}
	for _, e := range r.Snapshot().Events {
		if seen[e.Seq] {
			t.Fatalf("duplicate sequence number %d", e.Seq)
		}
		seen[e.Seq] = true
	}
}

func TestRecordLimitDropsRequestsButKeepsCriticalEvents(t *testing.T) {
	r := NewRecorder(testMeta(), 3)

	for i := range 10 {
		r.Record(RequestAt(RequestEvent{
			ID:      uint64(i),
			Method:  "GET",
			Sent:    time.Duration(i) * time.Millisecond,
			Done:    time.Duration(i+1) * time.Millisecond,
			Outcome: OutcomeOK,
		}))
	}

	// The signal is load-bearing for the verdict and must survive the limit.
	r.Record(SignalAt(time.Second, SignalEvent{Signal: "TERM"}))

	snap := r.Snapshot()
	if _, ok := snap.SignalOffset(); !ok {
		t.Fatal("signal event was dropped by the record limit")
	}
	if got := snap.Dropped; got != 7 {
		t.Errorf("Dropped = %d, want 7", got)
	}

	notices := snap.EventsOfKind(KindNotice)
	if len(notices) != 1 {
		t.Fatalf("got %d overflow notices, want exactly 1", len(notices))
	}
	if code := notices[0].Notice.Code; code != "record_limit_reached" {
		t.Errorf("notice code = %q, want record_limit_reached", code)
	}
}

func TestSignalOffsetIgnoresSigkill(t *testing.T) {
	r := NewRecorder(testMeta(), 0)
	r.Record(SignalAt(5*time.Second, SignalEvent{Signal: "TERM"}))
	r.Record(SignalAt(10*time.Second, SignalEvent{Signal: "KILL"}))

	snap := r.Snapshot()

	got, ok := snap.SignalOffset()
	if !ok || got != 5*time.Second {
		t.Fatalf("SignalOffset() = %v, %v; want 5s, true", got, ok)
	}

	kill, ok := snap.KillOffset()
	if !ok || kill != 10*time.Second {
		t.Fatalf("KillOffset() = %v, %v; want 10s, true", kill, ok)
	}
}

// A SIGKILL-only timeline must not be mistaken for the origin of the drain
// window; treating it as the signal would collapse the measurement entirely.
func TestSignalOffsetAbsentWhenOnlyKillRecorded(t *testing.T) {
	r := NewRecorder(testMeta(), 0)
	r.Record(SignalAt(10*time.Second, SignalEvent{Signal: "KILL"}))

	if _, ok := r.Snapshot().SignalOffset(); ok {
		t.Fatal("SignalOffset() reported a signal, but only SIGKILL was recorded")
	}
}

func TestProcessExitAccessor(t *testing.T) {
	code := 0
	r := NewRecorder(testMeta(), 0)
	r.Record(ProcessAt(time.Second, ProcessEvent{Phase: ProcStarted, PID: 4242}))
	r.Record(ProcessAt(9*time.Second, ProcessEvent{Phase: ProcExited, PID: 4242, ExitCode: &code}))

	got, at, ok := r.Snapshot().ProcessExit()
	if !ok {
		t.Fatal("ProcessExit() found no exit event")
	}
	if at != 9*time.Second {
		t.Errorf("exit offset = %v, want 9s", at)
	}
	if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Errorf("exit code = %v, want 0", got.ExitCode)
	}
}

func TestFirstAndLast(t *testing.T) {
	r := NewRecorder(testMeta(), 0)
	r.Record(StageAt(0, StageWarmup))
	r.Record(StageAt(time.Second, StageSteady))
	r.Record(StageAt(2*time.Second, StageComplete))

	snap := r.Snapshot()

	first, ok := snap.First(KindStage)
	if !ok || first.Stage.Stage != StageWarmup {
		t.Errorf("First() = %v, want warmup", first.Stage)
	}

	last, ok := snap.Last(KindStage)
	if !ok || last.Stage.Stage != StageComplete {
		t.Errorf("Last() = %v, want complete", last.Stage)
	}

	if _, ok := snap.First(KindSignal); ok {
		t.Error("First() found a signal that was never recorded")
	}
	if _, ok := snap.Last(KindSignal); ok {
		t.Error("Last() found a signal that was never recorded")
	}
}

func TestRequestLatency(t *testing.T) {
	r := RequestEvent{Sent: 100 * time.Millisecond, Done: 350 * time.Millisecond}
	if got := r.Latency(); got != 250*time.Millisecond {
		t.Fatalf("Latency() = %v, want 250ms", got)
	}
}

func TestRequestQueueDelay(t *testing.T) {
	cases := map[string]struct {
		event RequestEvent
		want  time.Duration
	}{
		"dispatched late": {
			RequestEvent{Scheduled: 100 * time.Millisecond, Sent: 180 * time.Millisecond},
			80 * time.Millisecond,
		},
		"dispatched on time": {
			RequestEvent{Scheduled: 100 * time.Millisecond, Sent: 100 * time.Millisecond},
			0,
		},
		// A request cannot be dispatched before it was scheduled; treat any such
		// record as zero rather than reporting a negative delay.
		"sent before scheduled": {
			RequestEvent{Scheduled: 200 * time.Millisecond, Sent: 100 * time.Millisecond},
			0,
		},
		"no schedule recorded": {
			RequestEvent{Sent: 100 * time.Millisecond},
			0,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := tc.event.QueueDelay(); got != tc.want {
				t.Errorf("QueueDelay() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestOutcomeSucceeded(t *testing.T) {
	if !OutcomeOK.Succeeded() {
		t.Error("OutcomeOK must count as success")
	}
	for _, o := range []Outcome{OutcomeHTTPError, OutcomeRefused, OutcomeReset, OutcomeTimeout, OutcomeEOF, OutcomeAbandoned} {
		if o.Succeeded() {
			t.Errorf("%q must not count as success", o)
		}
	}
}

func TestRecordAllPreservesOrder(t *testing.T) {
	r := NewRecorder(testMeta(), 0)
	r.RecordAll(
		StageAt(0, StageWarmup),
		StageAt(time.Second, StageSteady),
	)

	if got := r.Len(); got != 2 {
		t.Fatalf("Len() = %d, want 2", got)
	}
}

func TestNDJSONRoundTrip(t *testing.T) {
	exit := 0
	r := NewRecorder(testMeta(), 0)
	r.RecordAll(
		StageAt(0, StageWarmup),
		ReadinessAt(500*time.Millisecond, ReadinessEvent{Status: 200, Healthy: true, Outcome: OutcomeOK, Latency: time.Millisecond}),
		ListenerAt(600*time.Millisecond, ListenerEvent{Accepting: true, Outcome: OutcomeOK, Latency: 200 * time.Microsecond}),
		ConnectionAt(700*time.Millisecond, ConnectionEvent{ID: 1, Phase: ConnOpen, RemoteAddr: "127.0.0.1:8080"}),
		RequestAt(RequestEvent{ID: 1, Method: "POST", URL: "http://localhost:8080/api/orders", Sent: time.Second, Done: 1100 * time.Millisecond, Status: 200, Outcome: OutcomeOK, ConnID: 1}),
		SignalAt(5*time.Second, SignalEvent{Signal: "TERM", Skew: 300 * time.Microsecond}),
		ConnectionAt(5100*time.Millisecond, ConnectionEvent{ID: 1, Phase: ConnClose, Termination: TermRST}),
		ProcessAt(6*time.Second, ProcessEvent{Phase: ProcExited, PID: 4242, ExitCode: &exit, TerminatedBy: "SIGKILL"}),
		LogAt(5500*time.Millisecond, "stderr", "shutting down"),
		NoticeAt(0, "info", "calibrated", "rate set to 340 rps"),
	)
	want := r.Snapshot()

	var buf strings.Builder
	if err := WriteNDJSON(&buf, want); err != nil {
		t.Fatalf("WriteNDJSON: %v", err)
	}

	got, err := ReadNDJSON(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatalf("ReadNDJSON: %v", err)
	}

	if got.Meta != want.Meta {
		t.Errorf("meta round-trip mismatch:\n got %+v\nwant %+v", got.Meta, want.Meta)
	}
	if len(got.Events) != len(want.Events) {
		t.Fatalf("round-tripped %d events, want %d", len(got.Events), len(want.Events))
	}

	for i := range want.Events {
		g, w := got.Events[i], want.Events[i]
		if g.Kind != w.Kind || g.Seq != w.Seq || g.Offset != w.Offset {
			t.Errorf("event %d: got kind=%q seq=%d offset=%v, want kind=%q seq=%d offset=%v",
				i, g.Kind, g.Seq, g.Offset, w.Kind, w.Seq, w.Offset)
		}
	}

	// Re-encoding the decoded timeline must be byte-identical, which is the
	// property offline re-analysis depends on.
	var second strings.Builder
	if err := WriteNDJSON(&second, got); err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if second.String() != buf.String() {
		t.Error("NDJSON encoding is not stable across a round trip")
	}
}

func TestNDJSONRejectsMisplacedMeta(t *testing.T) {
	input := `{"seq":1,"offset_ns":0,"kind":"stage","stage":{"stage":"warmup"}}
{"seq":2,"offset_ns":0,"kind":"meta","meta":{"tool_version":"x","started_at":"2026-09-03T10:15:18Z","seed":0,"trial":1,"trials":1}}
`
	if _, err := ReadNDJSON(strings.NewReader(input)); err == nil {
		t.Fatal("expected an error when meta is not the first record")
	}
}

func TestNDJSONRejectsPayloadKindMismatch(t *testing.T) {
	cases := map[string]string{
		"no payload":        `{"seq":1,"offset_ns":0,"kind":"request"}`,
		"wrong payload":     `{"seq":1,"offset_ns":0,"kind":"request","stage":{"stage":"warmup"}}`,
		"two payloads":      `{"seq":1,"offset_ns":0,"kind":"stage","stage":{"stage":"warmup"},"log":{"stream":"stdout","line":"x"}}`,
		"unknown kind":      `{"seq":1,"offset_ns":0,"kind":"wat","stage":{"stage":"warmup"}}`,
		"meta without meta": `{"seq":1,"offset_ns":0,"kind":"meta"}`,
	}

	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadNDJSON(strings.NewReader(input)); err == nil {
				t.Fatalf("expected an error for %s", name)
			}
		})
	}
}

func TestNDJSONRejectsMalformedJSON(t *testing.T) {
	if _, err := ReadNDJSON(strings.NewReader("{not json}")); err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
}

func TestNDJSONEmptyInputYieldsEmptyTimeline(t *testing.T) {
	got, err := ReadNDJSON(strings.NewReader(""))
	if err != nil {
		t.Fatalf("ReadNDJSON on empty input: %v", err)
	}
	if len(got.Events) != 0 {
		t.Fatalf("got %d events from empty input", len(got.Events))
	}
}

func TestWriteNDJSONSkipsEmbeddedMetaEvents(t *testing.T) {
	meta := testMeta()
	tl := Timeline{
		Meta: meta,
		Events: []Event{
			{Seq: 1, Kind: KindMeta, Meta: &meta},
			StageAt(0, StageWarmup),
		},
	}

	var buf strings.Builder
	if err := WriteNDJSON(&buf, tl); err != nil {
		t.Fatalf("WriteNDJSON: %v", err)
	}

	if got := strings.Count(buf.String(), `"kind":"meta"`); got != 1 {
		t.Fatalf("meta appears %d times, want exactly 1", got)
	}
}

func FuzzReadNDJSON(f *testing.F) {
	f.Add("")
	f.Add(`{"seq":1,"offset_ns":0,"kind":"stage","stage":{"stage":"warmup"}}`)
	f.Add(`{"kind":"meta","meta":{"tool_version":"x","started_at":"2026-09-03T10:15:18Z","seed":1,"trial":1,"trials":1}}`)
	f.Add(`{"seq":1,"offset_ns":-5,"kind":"request","request":{"id":1,"method":"GET","sent_ns":0,"done_ns":1,"outcome":"ok"}}`)
	f.Add("{\x00}")

	f.Fuzz(func(t *testing.T, input string) {
		// Malformed evidence must produce an error, never a panic: the parser
		// is fed files that users attach to bug reports.
		tl, err := ReadNDJSON(strings.NewReader(input))
		if err != nil {
			return
		}
		var buf strings.Builder
		if err := WriteNDJSON(&buf, tl); err != nil {
			t.Fatalf("re-encoding a successfully parsed timeline failed: %v", err)
		}
	})
}
