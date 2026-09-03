package report

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/shutdowncheck/shutdowncheck/internal/analyze"
	"github.com/shutdowncheck/shutdowncheck/internal/timeline"
	"github.com/shutdowncheck/shutdowncheck/pkg/schema"
)

// Options control terminal rendering.
type Options struct {
	Width      int
	ForceColor bool
	NoColor    bool
	NoTimeline bool
	Quiet      bool
}

// DefaultWidth is used when no width is supplied and COLUMNS is unset.
const DefaultWidth = 88

const labelWidth = 9

// Human writes the terminal report.
func Human(w io.Writer, result analyze.Result, tl timeline.Timeline, opts Options) error {
	p := NewPalette(w, opts.ForceColor, opts.NoColor)
	width := opts.Width
	if width <= 0 {
		width = DefaultWidth
	}

	r := &humanReport{w: w, p: p, width: width, result: result, tl: tl}

	r.header()
	if !opts.NoTimeline {
		r.timeline()
	}
	r.requests()
	r.connections()
	r.verdict()
	r.findings()

	return r.err
}

type humanReport struct {
	w      io.Writer
	p      Palette
	width  int
	result analyze.Result
	tl     timeline.Timeline
	err    error
}

func (r *humanReport) printf(format string, args ...any) {
	if r.err != nil {
		return
	}
	_, r.err = fmt.Fprintf(r.w, format, args...)
}

func (r *humanReport) line(s string) { r.printf("%s\n", s) }
func (r *humanReport) blank()        { r.printf("\n") }

func (r *humanReport) header() {
	rep := r.result.Report

	title := "ShutdownCheck"
	if rep.ToolVersion != "" {
		title += " " + rep.ToolVersion
	}

	meta := []string{"profile=" + rep.Run.Profile}
	if rep.Target.Label != "" {
		meta = append(meta, "target="+rep.Target.Label)
	}
	if rep.Run.Trials.Total > 1 {
		meta = append(meta, fmt.Sprintf("trials=%d", rep.Run.Trials.Total))
	}

	r.line(r.p.Bold(title) + "   " + r.p.Dim(strings.Join(meta, "   ")))

	if rep.Probe.URL != "" {
		rate := "fixed rate"
		if rep.Load.Calibrated {
			rate = "calibrated"
		}
		r.line(fmt.Sprintf("  %s %s   %s to %.0f rps (%d in flight at the signal)",
			rep.Probe.Method, rep.Probe.URL, rate, rep.Load.RPS, rep.Load.ObservedInFlightAtSignal))
	}
	r.blank()
}

// timeline draws the run as parallel tracks against one shared axis.
//
// Reading five separate tables and mentally aligning them is the hard part of
// diagnosing a shutdown; putting traffic, readiness, the listener and the
// process on the same time axis is what makes the failure obvious at a glance.
func (r *humanReport) timeline() {
	facts := r.result.Facts
	if !facts.HasSignal || len(facts.Requests) == 0 {
		return
	}

	cols := r.width - labelWidth
	if cols < 20 {
		return
	}

	start, end := r.span()
	if end <= start {
		return
	}

	at := func(offset time.Duration) int {
		col := int(float64(offset-start) / float64(end-start) * float64(cols-1))
		return clamp(col, 0, cols-1)
	}

	r.line(r.p.Bold("TIMELINE") + r.p.Dim(strings.Repeat(" ", maxInt(1, cols-24))+"T=0 is the signal"))
	r.line(pad("", labelWidth) + r.axis(start, end, cols, at(facts.SignalAt)))

	r.track("traffic", r.trafficRow(cols, start, end))
	if facts.HadReadinessProbe {
		r.track("ready", r.sampleRow(cols, at, timeline.KindReadiness))
	}
	if facts.HadListenerProbe {
		r.track("listen", r.sampleRow(cols, at, timeline.KindListener))
	}
	r.track("process", r.processRow(cols, at))
	r.track("budget", r.budgetRow(cols, at))
	r.blank()
}

// span is the window the timeline covers, always including the signal and
// whatever happened after it.
func (r *humanReport) span() (time.Duration, time.Duration) {
	facts := r.result.Facts
	start, end := facts.SignalAt, facts.SignalAt

	for _, req := range facts.Requests {
		if req.Sent < start {
			start = req.Sent
		}
		if req.Done > end {
			end = req.Done
		}
	}
	for _, at := range []*time.Duration{facts.ExitAt, facts.KillAt, facts.ListenerClosedAt} {
		if at != nil && *at > end {
			end = *at
		}
	}
	return start, end
}

func (r *humanReport) axis(start, end time.Duration, cols, signalCol int) string {
	ticks := []byte(strings.Repeat("-", cols))
	ticks[0] = '|'
	ticks[cols-1] = '|'
	if signalCol > 0 && signalCol < cols-1 {
		ticks[signalCol] = '|'
	}

	labels := []byte(strings.Repeat(" ", cols))
	place := func(col int, text string) {
		at := clamp(col-len(text)/2, 0, cols-len(text))
		copy(labels[at:], text)
	}
	place(0, relLabel(start-r.result.Facts.SignalAt))
	place(signalCol, "0s")
	place(cols-1, relLabel(end-r.result.Facts.SignalAt))

	return r.p.Dim(string(labels)) + "\n" + pad("", labelWidth) + r.p.Dim(string(ticks))
}

func (r *humanReport) track(label, row string) {
	r.line(r.p.Dim(pad(" "+label, labelWidth)) + row)
}

// trafficRow shades each column by how the requests overlapping it fared.
func (r *humanReport) trafficRow(cols int, start, end time.Duration) string {
	ok := make([]int, cols)
	failed := make([]int, cols)
	step := (end - start) / time.Duration(cols)
	if step <= 0 {
		step = 1
	}

	for _, req := range r.result.Facts.Requests {
		from := clamp(int((req.Sent-start)/step), 0, cols-1)
		to := clamp(int((req.Done-start)/step), 0, cols-1)
		for c := from; c <= to; c++ {
			if req.Outcome.Succeeded() {
				ok[c]++
			} else {
				failed[c]++
			}
		}
	}

	var b strings.Builder
	for c := range cols {
		switch {
		case ok[c] == 0 && failed[c] == 0:
			b.WriteString(" ")
		case failed[c] == 0:
			b.WriteString(r.p.Green("\u2588"))
		case ok[c] == 0:
			b.WriteString(r.p.Red("\u2591"))
		default:
			b.WriteString(r.p.Yellow("\u2593"))
		}
	}
	return b.String()
}

// sampleRow renders a boolean observation series, carrying the last known value
// forward so gaps between polls do not read as missing data.
func (r *humanReport) sampleRow(cols int, at func(time.Duration) int, kind timeline.Kind) string {
	state := make([]int, cols) // 0 unknown, 1 up, -1 down
	for _, e := range r.tl.EventsOfKind(kind) {
		up := false
		switch kind {
		case timeline.KindReadiness:
			up = e.Readiness.Healthy
		case timeline.KindListener:
			up = e.Listener.Accepting
		}

		col := at(e.Offset)
		if up {
			state[col] = 1
		} else {
			state[col] = -1
		}
	}

	var b strings.Builder
	last := 0
	for c := range cols {
		if state[c] != 0 {
			last = state[c]
		}
		switch last {
		case 1:
			b.WriteString(r.p.Green("\u2500"))
		case -1:
			b.WriteString(r.p.Red("\u00b7"))
		default:
			b.WriteString(" ")
		}
	}
	return b.String()
}

func (r *humanReport) processRow(cols int, at func(time.Duration) int) string {
	facts := r.result.Facts

	exitCol := cols
	if facts.ExitAt != nil {
		exitCol = at(*facts.ExitAt)
	}

	var b strings.Builder
	for c := range cols {
		switch {
		case c < exitCol:
			b.WriteString(r.p.Green("\u2500"))
		case c == exitCol:
			b.WriteString(r.p.Red("\u2573"))
		default:
			b.WriteString(" ")
		}
	}

	suffix := "  still running"
	if facts.ExitAt != nil {
		suffix = "  exit at " + relLabel(*facts.ExitAt-facts.SignalAt)
	}
	return b.String() + r.p.Dim(suffix)
}

func (r *humanReport) budgetRow(cols int, at func(time.Duration) int) string {
	facts := r.result.Facts
	grace := time.Duration(r.result.Report.Target.GracePeriodMS) * time.Millisecond

	from := at(facts.SignalAt)
	to := at(facts.SignalAt + grace)

	var b strings.Builder
	for c := range cols {
		switch {
		case c < from:
			b.WriteString(" ")
		case c == from || c == to:
			b.WriteString(r.p.Dim("|"))
		case c < to:
			b.WriteString(r.p.Dim("="))
		default:
			b.WriteString(" ")
		}
	}

	suffix := fmt.Sprintf("  grace %s", grace)
	if facts.KillAt != nil {
		suffix += r.p.Red("  SIGKILL sent")
	}
	return b.String() + r.p.Dim(suffix)
}

func (r *humanReport) requests() {
	rep := r.result.Report
	r.line(r.p.Bold("REQUESTS"))

	rows := []struct {
		label string
		stats schema.PhaseStats
	}{
		{"before the signal", rep.Requests.ByPhase.Steady},
		{"in flight at signal", rep.Requests.ByPhase.InFlight},
		{"after the signal", rep.Requests.ByPhase.PostSignal},
		{"past the window", rep.Requests.ByPhase.PostWindow},
	}

	for _, row := range rows {
		if row.stats.Count == 0 {
			continue
		}

		failed := padLeft(thousands(row.stats.Failed), 6) + " failed"
		if row.stats.Failed > 0 {
			failed = r.p.Red(failed)
		} else {
			failed = r.p.Dim(failed)
		}

		r.line(fmt.Sprintf("  %s %s %s %s%s",
			pad(row.label, 21),
			padLeft(thousands(row.stats.Count), 7),
			padLeft(thousands(row.stats.OK), 7)+" ok",
			failed,
			r.p.Dim(failureBreakdown(row.stats.Failures)),
		))
	}

	if n := rep.Requests.ByPhase.AtSigkill.Count; n > 0 {
		r.line("  " + r.p.Red(fmt.Sprintf("%s destroyed by SIGKILL", thousands(n))))
	}
	r.blank()
}

func failureBreakdown(failures map[string]int) string {
	if len(failures) == 0 {
		return ""
	}

	parts := make([]string, 0, len(failures))
	for _, outcome := range sortedKeys(failures) {
		parts = append(parts, fmt.Sprintf("%d %s", failures[outcome], outcome))
	}
	return "   (" + strings.Join(parts, ", ") + ")"
}

func (r *humanReport) connections() {
	conns := r.result.Report.Connections
	if conns.Opened == 0 && conns.ReusedAfterSignal == 0 {
		return
	}

	r.line(r.p.Bold("CONNECTIONS"))
	r.line(fmt.Sprintf("  %s %s", pad("opened", 40), padLeft(thousands(conns.Opened), 7)))

	if conns.ReusedAfterSignal > 0 {
		r.line(fmt.Sprintf("  %s %s", pad("reused after the signal", 40), padLeft(thousands(conns.ReusedAfterSignal), 7)))

		note := ""
		if conns.ClosedWithConnectionClose == 0 {
			note = r.p.Yellow("   clients were never told to close")
		}
		r.line(fmt.Sprintf("  %s %s%s",
			pad("closed with Connection: close", 40),
			padLeft(thousands(conns.ClosedWithConnectionClose), 7), note))
	}

	if conns.ClosedByRST > 0 {
		r.line(fmt.Sprintf("  %s %s%s",
			pad("terminated by RST rather than FIN", 40),
			padLeft(thousands(conns.ClosedByRST), 7),
			r.p.Red("   abrupt")))
	}
	r.blank()
}

func (r *humanReport) verdict() {
	rep := r.result.Report

	var badge string
	switch rep.Verdict {
	case schema.VerdictPass:
		badge = r.p.Green(r.p.Bold("VERDICT: PASS"))
	case schema.VerdictFail:
		badge = r.p.Red(r.p.Bold("VERDICT: FAIL"))
	default:
		badge = r.p.Yellow(r.p.Bold("VERDICT: INCONCLUSIVE"))
	}

	parts := []string{badge, fmt.Sprintf("score %d/100 (%s)", rep.Score.Value, rep.Score.Grade)}
	if rep.Run.Trials.Total > 1 {
		consistency := fmt.Sprintf("%d of %d trials failed", rep.Run.Trials.Failed, rep.Run.Trials.Total)
		if !rep.Run.Trials.Consistent {
			consistency = r.p.Yellow(consistency + " — flaky")
		}
		parts = append(parts, consistency)
	}
	r.line(strings.Join(parts, "      "))

	for _, gate := range rep.Gates {
		if !gate.Passed {
			r.line("  " + r.p.Red(fmt.Sprintf("gate %s: %.4g exceeds the limit of %.4g",
				gate.Name, gate.Actual, gate.Threshold)))
		}
	}
	r.blank()
}

func (r *humanReport) findings() {
	findings := r.result.Report.Findings
	if len(findings) == 0 {
		r.line(r.p.Green("  Nothing to report: this service terminates correctly."))
		return
	}

	for _, finding := range findings {
		marker, colour := "!", r.p.Yellow
		if finding.Severity == schema.SeverityError {
			marker, colour = "x", r.p.Red
		}

		r.line(fmt.Sprintf("  %s %s  %s", colour(marker), colour(pad(finding.ID, 6)), r.p.Bold(finding.Name)))
		r.line("      " + finding.Summary)
		if finding.Impact != "" {
			r.line("      " + r.p.Dim(finding.Impact))
		}
	}

	r.blank()
	if len(findings) > 0 {
		r.line(r.p.Dim(fmt.Sprintf("  Run `shutdowncheck explain %s` for how to fix this.", findings[0].ID)))
	}
}

func relLabel(d time.Duration) string {
	sign := "+"
	if d < 0 {
		sign, d = "-", -d
	}
	if d == 0 {
		return "0s"
	}
	return sign + d.Truncate(100*time.Millisecond).String()
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
