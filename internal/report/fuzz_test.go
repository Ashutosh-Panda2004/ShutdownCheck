package report_test

import (
	"bytes"
	"testing"

	"github.com/shutdowncheck/shutdowncheck/internal/analyze"
	"github.com/shutdowncheck/shutdowncheck/internal/report"
	"github.com/shutdowncheck/shutdowncheck/internal/timeline"
)

// Rendering is where arbitrary recorded evidence meets width arithmetic. The
// human timeline maps offsets onto a fixed number of columns, and a recording
// with absurd offsets, a zero-length run or events out of order is exactly the
// shape that turns into an index-out-of-range in that arithmetic.
//
// The input is NDJSON rather than a constructed Timeline so the fuzzer explores
// the parser and the renderers together, which is the path a real recording
// takes on its way through `analyze`.
func FuzzRenderEveryFormat(f *testing.F) {
	seedRenderCorpus(f)

	f.Fuzz(func(t *testing.T, data []byte, width int) {
		tl, err := timeline.ReadNDJSON(bytes.NewReader(data))
		if err != nil {
			return // not a recording; the parser has its own fuzz target
		}

		policy, err := analyze.PolicyFor(analyze.ProfileStandalone)
		if err != nil {
			t.Fatalf("PolicyFor: %v", err)
		}

		result := analyze.Analyze(analyze.Input{Timeline: tl, Policy: policy})

		// Every format a user can ask for. A crash in any of them is a crash
		// after the measurement is already taken, which loses evidence that
		// cannot be recreated.
		var buf bytes.Buffer
		_ = report.JSON(&buf, result.Report)
		buf.Reset()
		_ = report.NDJSON(&buf, tl)
		buf.Reset()
		_ = report.JUnit(&buf, result)
		buf.Reset()
		_ = report.Markdown(&buf, result)
		buf.Reset()
		_ = report.Badge(&buf, result.Report)
		buf.Reset()
		_ = report.Human(&buf, result, tl, report.Options{Width: width, NoColor: true})
	})
}

func seedRenderCorpus(f *testing.F) {
	f.Helper()

	// A well-formed minimal recording.
	f.Add([]byte(`{"kind":"meta","tool_version":"test","profile":"standalone"}
{"kind":"signal","offset_ms":1000,"signal":"TERM"}
{"kind":"process","offset_ms":2000,"phase":"exited","exit_code":0}
`), 100)

	// Degenerate widths, which is where the column arithmetic breaks.
	f.Add([]byte(`{"kind":"meta","tool_version":"t"}`), 0)
	f.Add([]byte(`{"kind":"meta","tool_version":"t"}`), -1)
	f.Add([]byte(`{"kind":"meta","tool_version":"t"}`), 1)

	// Everything at the same instant, so the timeline spans zero duration.
	f.Add([]byte(`{"kind":"meta","tool_version":"t"}
{"kind":"signal","offset_ms":0,"signal":"TERM"}
{"kind":"process","offset_ms":0,"phase":"exited","exit_code":0}
`), 80)

	// A request that finishes before it starts, and one at an extreme offset.
	f.Add([]byte(`{"kind":"meta","tool_version":"t"}
{"kind":"request","offset_ms":10,"sent_ms":10,"done_ms":1,"outcome":"ok","status":200}
{"kind":"request","offset_ms":9223372036854,"sent_ms":9223372036854,"done_ms":9223372036854,"outcome":"ok"}
{"kind":"signal","offset_ms":5,"signal":"TERM"}
`), 80)

	f.Add([]byte(""), 80)
}
