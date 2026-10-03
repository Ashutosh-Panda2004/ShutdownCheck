package report

import (
	"fmt"
	"html"
	"html/template"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/analyze"
	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/redact"
	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/remediate"
	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/timeline"
	"github.com/Ashutosh-Panda2004/ShutdownCheck/pkg/schema"
)

// HTML writes a self-contained visual report: verdict banner, score gauge, the
// seven-stage timeline, one dot per request, the findings, and a fix panel.
// Everything is inline (no external assets), so the file works from file://.
//
// The timeline and request dots are drawn from the raw evidence plus the
// analysed facts, not from the aggregate JSON alone: the aggregate report has
// no per-request records.
func HTML(w io.Writer, result analyze.Result, tl timeline.Timeline, _ Options) error {
	vm := buildHTMLModel(result, tl)
	return htmlPage.Execute(w, vm)
}

// ---------------------------------------------------------------------------
// View model
// ---------------------------------------------------------------------------

type htmlBand struct {
	X     float64
	Width float64
	Label string
	Class string
}

type htmlMarker struct {
	X     float64
	Label string
	Sub   string
	Class string
}

type htmlDot struct {
	CX    float64
	CY    float64
	Fill  string
	Title string
}

type htmlFinding struct {
	ID       string
	Name     string
	Severity string
	Stage    string
	Summary  string
	Impact   string
	Evidence []htmlEvidence
}

type htmlEvidence struct {
	Key   string
	Value string
}

type htmlFix struct {
	ID       string
	Name     string
	Severity string
	Open     bool
	Body     template.HTML
}

type htmlModel struct {
	Title          string
	Verdict        string
	VerdictLabel   string
	VerdictSummary string
	Score          int
	Grade          string
	GaugeDash      float64
	GaugeColor     string
	Target         string
	Profile        string
	ToolVersion    string
	StartedAt      string
	SchemaVersion  string

	AxisW   float64
	AxisEnd float64
	Bands   []htmlBand
	Markers []htmlMarker
	Dots    []htmlDot
	Lanes   []htmlLane
	DotNote string

	Findings []htmlFinding
	Fixes    []htmlFix
	Gates    []schema.Gate
	HasGates bool
}

type htmlLane struct {
	Y     float64
	Label string
	Color string
}

const htmlAxisW = 1000.0

func buildHTMLModel(result analyze.Result, tl timeline.Timeline) htmlModel {
	rep := result.Report
	f := result.Facts

	vm := htmlModel{
		Verdict:       string(rep.Verdict),
		Score:         rep.Score.Value,
		Grade:         rep.Score.Grade,
		Target:        rep.Target.Label,
		Profile:       rep.Run.Profile,
		ToolVersion:   rep.ToolVersion,
		SchemaVersion: rep.SchemaVersion,
		AxisW:         htmlAxisW,
		AxisEnd:       htmlAxisW - 8,
	}
	if vm.Grade == "" {
		vm.Grade = schema.Grade(vm.Score)
	}
	if !rep.Run.StartedAt.IsZero() {
		vm.StartedAt = rep.Run.StartedAt.Format("2006-01-02 15:04:05 MST")
	}

	switch rep.Verdict {
	case schema.VerdictPass:
		vm.VerdictLabel = "PASS"
		vm.VerdictSummary = "Correct termination behaviour was positively demonstrated."
	case schema.VerdictFail:
		vm.VerdictLabel = "FAIL"
		vm.VerdictSummary = "At least one error-severity signature fired or a gate threshold was breached."
	default:
		vm.VerdictLabel = "INCONCLUSIVE"
		vm.VerdictSummary = "The experiment was not valid — this is not a pass."
	}
	vm.Title = fmt.Sprintf("ShutdownCheck report — %s (%s)", vm.VerdictLabel, vm.Target)

	// Score gauge: SVG circle arc.
	const r = 52.0
	const circ = 2 * 3.141592653589793 * r
	vm.GaugeDash = circ * (1 - float64(vm.Score)/100)
	switch vm.Grade {
	case "A", "B":
		vm.GaugeColor = "#1a7f37"
	case "C":
		vm.GaugeColor = "#9a6700"
	case "D":
		vm.GaugeColor = "#b45309"
	default:
		vm.GaugeColor = "#b42318"
	}

	// ---- timeline axis ----
	tEnd := runEnd(f, tl)
	if tEnd <= 0 {
		tEnd = time.Second
	}
	x := func(d time.Duration) float64 {
		if d < 0 {
			d = 0
		}
		if d > tEnd {
			d = tEnd
		}
		return 8 + (htmlAxisW-16)*float64(d)/float64(tEnd)
	}

	// Bands from the tool's own stage events: warmup, steady, drain.
	stageAt := map[timeline.Stage]time.Duration{}
	for _, e := range tl.EventsOfKind(timeline.KindStage) {
		if e.Stage != nil {
			if _, ok := stageAt[e.Stage.Stage]; !ok {
				stageAt[e.Stage.Stage] = e.Offset
			}
		}
	}
	var bands []htmlBand
	addBand := func(from, to time.Duration, label, class string) {
		if to <= from {
			return
		}
		bands = append(bands, htmlBand{X: x(from), Width: x(to) - x(from), Label: label, Class: class})
	}
	sigAt := time.Duration(-1)
	if f.HasSignal {
		sigAt = f.SignalAt
	}
	if w0, ok := stageAt[timeline.StageWarmup]; ok {
		s1 := sigAt
		if s0, ok := stageAt[timeline.StageSteady]; ok && (s1 < 0 || s0 < s1) {
			addBand(w0, s0, "Warmup", "band-warmup")
			addBand(s0, atOr(s1, tEnd, tEnd), "Steady", "band-steady")
		} else {
			addBand(w0, atOr(s1, tEnd, tEnd), "Warmup", "band-warmup")
		}
	}
	if sigAt >= 0 {
		drainEnd := tEnd
		for _, d := range []time.Duration{ptrDur(f.ListenerClosedAt), ptrDur(f.ExitAt), ptrDur(f.KillAt)} {
			if d >= 0 && d < drainEnd {
				drainEnd = d
			}
		}
		addBand(sigAt, drainEnd, "Drain", "band-drain")
	}
	vm.Bands = bands

	// Milestones: the seven stages of the run story.
	var markers []htmlMarker
	addMarker := func(d time.Duration, ok bool, label, sub, class string) {
		if !ok {
			return
		}
		markers = append(markers, htmlMarker{X: x(d), Label: label, Sub: sub, Class: class})
	}
	addMarker(sigAt, f.HasSignal, "SIGTERM", fmtDur(sigAt), "mk-signal")
	addMarker(ptrDur(f.ReadinessFlippedAt), f.ReadinessFlippedAt != nil, "Readiness flipped", fmtDur(ptrDur(f.ReadinessFlippedAt)), "mk-ready")
	addMarker(ptrDur(f.ListenerClosedAt), f.ListenerClosedAt != nil, "Listener closed", fmtDur(ptrDur(f.ListenerClosedAt)), "mk-listener")
	addMarker(ptrDur(f.ExitAt), f.HasExit, "Process exited", fmtDur(ptrDur(f.ExitAt)), "mk-exit")
	addMarker(ptrDur(f.KillAt), f.KillAt != nil, "SIGKILL", fmtDur(ptrDur(f.KillAt)), "mk-kill")
	vm.Markers = markers

	// ---- request dots ----
	lanes := []htmlLane{
		{Y: 88, Label: "served", Color: "#2da44e"},
		{Y: 110, Label: "http error", Color: "#9a6700"},
		{Y: 132, Label: "refused", Color: "#d97706"},
		{Y: 154, Label: "destroyed", Color: "#d1242f"},
	}
	vm.Lanes = lanes
	laneOf := func(o timeline.Outcome) int {
		switch o {
		case timeline.OutcomeOK:
			return 0
		case timeline.OutcomeHTTPError:
			return 1
		case timeline.OutcomeRefused:
			return 2
		default:
			return 3
		}
	}
	fillOf := func(o timeline.Outcome) string {
		switch o {
		case timeline.OutcomeOK:
			return "#2da44e"
		case timeline.OutcomeHTTPError:
			return "#9a6700"
		case timeline.OutcomeRefused:
			return "#d97706"
		default:
			return "#d1242f"
		}
	}
	reqs := f.Requests
	truncated := false
	if len(reqs) > 2500 {
		// Even sample so the shape of the run survives.
		step := float64(len(reqs)) / 2500
		kept := make([]analyze.ClassifiedRequest, 0, 2500)
		for i := 0; i < 2500; i++ {
			kept = append(kept, reqs[int(float64(i)*step)])
		}
		reqs = kept
		truncated = true
	}
	var dots []htmlDot
	for _, r := range reqs {
		lane := lanes[laneOf(r.Outcome)]
		// Deterministic vertical jitter inside the lane so simultaneous
		// requests do not hide each other.
		jitter := float64(r.ID%7) - 3
		dots = append(dots, htmlDot{
			CX:   x(r.Sent),
			CY:   lane.Y + jitter,
			Fill: fillOf(r.Outcome),
			Title: fmt.Sprintf("#%d · %s %s · %s · %s · phase %s",
				r.ID, r.Method, redact.URL(r.URL), r.Outcome, r.Latency().Round(time.Millisecond), r.Phase),
		})
	}
	vm.Dots = dots
	if truncated {
		vm.DotNote = fmt.Sprintf("showing an even sample of 2,500 of %d requests", len(f.Requests))
	} else {
		vm.DotNote = fmt.Sprintf("%d requests", len(f.Requests))
	}

	// ---- findings ----
	for _, fd := range rep.Findings {
		hf := htmlFinding{
			ID:       fd.ID,
			Name:     fd.Name,
			Severity: string(fd.Severity),
			Stage:    stageName(fd.Stage),
			Summary:  fd.Summary,
			Impact:   fd.Impact,
		}
		keys := make([]string, 0, len(fd.Evidence))
		for k := range fd.Evidence {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			hf.Evidence = append(hf.Evidence, htmlEvidence{Key: k, Value: fmt.Sprintf("%v", fd.Evidence[k])})
		}
		vm.Findings = append(vm.Findings, hf)
	}

	// ---- fix panel: error findings open, warnings collapsed ----
	for _, fd := range rep.Findings {
		if fd.Severity != schema.SeverityError && fd.Severity != schema.SeverityWarn {
			continue
		}
		body := explainHTML(analyze.SignatureID(fd.ID))
		vm.Fixes = append(vm.Fixes, htmlFix{
			ID:       fd.ID,
			Name:     fd.Name,
			Severity: string(fd.Severity),
			Open:     fd.Severity == schema.SeverityError,
			Body:     body,
		})
	}

	vm.Gates = rep.Gates
	vm.HasGates = len(rep.Gates) > 0
	return vm
}

func ptrDur(d *time.Duration) time.Duration {
	if d == nil {
		return -1
	}
	return *d
}

func atOr(d, fallback, _ time.Duration) time.Duration {
	if d < 0 {
		return fallback
	}
	return d
}

// runEnd is the right edge of the timeline: the latest observed event, so the
// axis always covers the whole story.
func runEnd(f analyze.Facts, tl timeline.Timeline) time.Duration {
	end := time.Duration(0)
	for _, e := range tl.Events {
		if e.Offset > end {
			end = e.Offset
		}
	}
	for _, d := range []time.Duration{ptrDur(f.ExitAt), ptrDur(f.KillAt)} {
		if d > end {
			end = d
		}
	}
	return end
}

func fmtDur(d time.Duration) string {
	if d < 0 {
		return "—"
	}
	if d >= time.Second {
		return fmt.Sprintf("%.2fs", d.Seconds())
	}
	return fmt.Sprintf("%dms", d.Milliseconds())
}

func stageName(s string) string {
	switch s {
	case "S1":
		return "S1 · Signal received"
	case "S2":
		return "S2 · Readiness flip"
	case "S3":
		return "S3 · Lame duck"
	case "S4":
		return "S4 · Listener closed"
	case "S5":
		return "S5 · Connection close"
	case "S6":
		return "S6 · Drain"
	case "S7":
		return "S7 · Clean exit"
	default:
		return "—"
	}
}

// explainHTML renders a signature's remediation page (Markdown) to sanitised
// HTML for the fix panel. It is a small purpose-built converter: the pages use
// headings, bold, inline code, fenced code blocks, tables and lists, and
// nothing else.
func explainHTML(id analyze.SignatureID) template.HTML {
	md, err := remediate.Explain(id)
	if err != nil {
		return template.HTML("<p>" + html.EscapeString(err.Error()) + "</p>") // #nosec G203 -- the error text is escaped before it is wrapped
	}
	return template.HTML(markdownToHTML(md)) // #nosec G203 -- markdownToHTML escapes every text and code span it emits, and md is the tool's own embedded remediation page
}

func markdownToHTML(md string) string {
	var b strings.Builder
	lines := strings.Split(md, "\n")

	flushList := func(items *[]string) {
		if len(*items) == 0 {
			return
		}
		b.WriteString("<ul>\n")
		for _, it := range *items {
			b.WriteString("<li>" + inlineMD(it) + "</li>\n")
		}
		b.WriteString("</ul>\n")
		*items = nil
	}
	flushTable := func(rows *[][]string) {
		if len(*rows) == 0 {
			return
		}
		b.WriteString("<table>\n<thead>\n<tr>")
		for _, c := range (*rows)[0] {
			b.WriteString("<th>" + inlineMD(c) + "</th>")
		}
		b.WriteString("</tr>\n</thead>\n<tbody>\n")
		for _, row := range (*rows)[1:] {
			b.WriteString("<tr>")
			for _, c := range row {
				b.WriteString("<td>" + inlineMD(c) + "</td>")
			}
			b.WriteString("</tr>\n")
		}
		b.WriteString("</tbody>\n</table>\n")
		*rows = nil
	}

	var list []string
	var table [][]string
	inFence := false
	fenceLang := ""
	var fence strings.Builder

	isSep := func(s string) bool {
		s = strings.TrimSpace(s)
		if !strings.HasPrefix(s, "|") {
			return false
		}
		inner := strings.Trim(s, "| ")
		for _, part := range strings.Split(inner, "|") {
			p := strings.TrimSpace(part)
			if len(p) < 3 {
				return false
			}
			for _, r := range p {
				if r != '-' && r != ':' {
					return false
				}
			}
		}
		return true
	}
	splitRow := func(s string) []string {
		parts := strings.Split(strings.TrimSpace(s), "|")
		var out []string
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" || len(parts) > 2 {
				out = append(out, p)
			}
		}
		// Drop the empty cells the leading/trailing pipes produce.
		for len(out) > 0 && out[0] == "" {
			out = out[1:]
		}
		for len(out) > 0 && out[len(out)-1] == "" {
			out = out[:len(out)-1]
		}
		return out
	}

	para := []string{}
	flushPara := func() {
		if len(para) == 0 {
			return
		}
		b.WriteString("<p>" + inlineMD(strings.Join(para, " ")) + "</p>\n")
		para = nil
	}

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "```") {
			flushPara()
			flushList(&list)
			flushTable(&table)
			if !inFence {
				inFence = true
				fenceLang = strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
				fence.Reset()
			} else {
				inFence = false
				b.WriteString("<pre><code")
				if fenceLang != "" {
					b.WriteString(` class="language-` + html.EscapeString(fenceLang) + `"`)
				}
				b.WriteString(">" + html.EscapeString(fence.String()) + "</code></pre>\n")
			}
			continue
		}
		if inFence {
			fence.WriteString(line + "\n")
			continue
		}
		if trimmed == "" {
			flushPara()
			flushList(&list)
			flushTable(&table)
			continue
		}
		if strings.HasPrefix(trimmed, "|") && i+1 < len(lines) && isSep(lines[i+1]) {
			flushPara()
			flushList(&list)
			table = append(table, splitRow(trimmed))
			i++ // consume separator
			continue
		}
		if strings.HasPrefix(trimmed, "|") && len(table) > 0 {
			table = append(table, splitRow(trimmed))
			continue
		}
		if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
			flushPara()
			flushTable(&table)
			list = append(list, strings.TrimSpace(trimmed[2:]))
			continue
		}
		if strings.HasPrefix(trimmed, "### ") {
			flushPara()
			flushList(&list)
			flushTable(&table)
			b.WriteString("<h4>" + inlineMD(strings.TrimSpace(trimmed[4:])) + "</h4>\n")
			continue
		}
		if strings.HasPrefix(trimmed, "## ") {
			flushPara()
			flushList(&list)
			flushTable(&table)
			b.WriteString("<h3>" + inlineMD(strings.TrimSpace(trimmed[3:])) + "</h3>\n")
			continue
		}
		if strings.HasPrefix(trimmed, "# ") {
			flushPara()
			flushList(&list)
			flushTable(&table)
			b.WriteString("<h3>" + inlineMD(strings.TrimSpace(trimmed[2:])) + "</h3>\n")
			continue
		}
		if strings.HasPrefix(trimmed, "> ") {
			flushPara()
			flushList(&list)
			flushTable(&table)
			b.WriteString("<blockquote><p>" + inlineMD(strings.TrimSpace(trimmed[2:])) + "</p></blockquote>\n")
			continue
		}
		para = append(para, trimmed)
	}
	flushPara()
	flushList(&list)
	flushTable(&table)
	return b.String()
}

// inlineMD handles code spans, bold, italic and links. Code spans are
// extracted first so formatting inside them is never interpreted.
func inlineMD(s string) string {
	s = html.EscapeString(s)
	var codes []string
	s = codeSpanRe.ReplaceAllStringFunc(s, func(m string) string {
		inner := m[1 : len(m)-1]
		codes = append(codes, "<code>"+inner+"</code>")
		return fmt.Sprintf("\x00%d\x00", len(codes)-1)
	})
	s = boldRe.ReplaceAllString(s, "<strong>$1</strong>")
	s = italicRe.ReplaceAllString(s, "<em>$1</em>")
	s = linkRe.ReplaceAllString(s, `<a href="$2" rel="noopener">$1</a>`)
	for i, c := range codes {
		s = strings.ReplaceAll(s, fmt.Sprintf("\x00%d\x00", i), c)
	}
	return s
}

var (
	codeSpanRe = regexp.MustCompile("`[^`\\n]+`")
	boldRe     = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	italicRe   = regexp.MustCompile(`\*([^*]+)\*`)
	linkRe     = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
)

// ---------------------------------------------------------------------------
// Page template: everything inline, no external assets.
// ---------------------------------------------------------------------------

var htmlPage = template.Must(template.New("report").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
:root{
  --ink:#1f2328; --muted:#59636e; --line:#d0d7de; --bg:#f6f8fa; --card:#ffffff;
  --green:#1a7f37; --red:#b42318; --amber:#9a6700; --blue:#0969da; --orange:#b45309;
}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--ink);
  font:16px/1.55 -apple-system,BlinkMacSystemFont,"Segoe UI",Helvetica,Arial,sans-serif}
.wrap{max-width:1120px;margin:0 auto;padding:0 20px 64px}
.skip{position:absolute;left:-9999px}
.skip:focus{left:8px;top:8px;background:#fff;padding:8px 12px;z-index:99}

/* verdict banner */
.banner{color:#fff;padding:28px 0 24px;margin:0 -20px 28px}
.banner.pass{background:var(--green)} .banner.fail{background:var(--red)}
.banner.inconclusive{background:var(--amber)}
.banner .wrap{padding-bottom:0}
.banner h1{margin:0 0 4px;font-size:30px;letter-spacing:.02em}
.banner p{margin:0;opacity:.94;max-width:70ch}
.meta{margin-top:14px;font-size:14px;opacity:.92}
.meta code{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}

/* layout */
.grid{display:grid;grid-template-columns:220px 1fr;gap:24px;align-items:start}
@media(max-width:760px){.grid{grid-template-columns:1fr}}
.card{background:var(--card);border:1px solid var(--line);border-radius:10px;
  padding:18px 20px;margin-bottom:24px}
.card h2{margin:0 0 12px;font-size:19px}
.card h2 .sub{font-weight:normal;color:var(--muted);font-size:14px}

/* score gauge */
.gauge{display:flex;gap:18px;align-items:center}
.gauge .num{font-size:15px;color:var(--muted)}
.gauge .num strong{font-size:26px;color:var(--ink)}

/* timeline svg */
.tl{width:100%;height:auto;display:block}
.band-warmup{fill:#ddf4ff} .band-steady{fill:#dafbe1} .band-drain{fill:#fff8c5}
.band-label{font-size:11px;fill:var(--muted);font-weight:600}
.mk-signal{stroke:#b42318} .mk-ready{stroke:#b45309} .mk-listener{stroke:#0969da}
.mk-exit{stroke:#1f2328} .mk-kill{stroke:#000}
.mk-label{font-size:11px;font-weight:600;fill:var(--ink)}
.mk-sub{font-size:10px;fill:var(--muted)}
.axis{stroke:#8b949e;stroke-width:1}
.dot{stroke:#fff;stroke-width:.6;opacity:.85}
.lane-label{font-size:10px;fill:var(--muted)}
.legend{display:flex;flex-wrap:wrap;gap:14px;font-size:13px;color:var(--muted);margin-top:8px}
.legend i{display:inline-block;width:10px;height:10px;border-radius:50%;margin-right:5px}

/* findings */
.finding{border:1px solid var(--line);border-radius:8px;padding:14px 16px;margin:0 0 12px;background:#fff}
.finding header{display:flex;flex-wrap:wrap;gap:8px;align-items:baseline;margin-bottom:6px}
.finding .id{font-family:ui-monospace,Menlo,Consolas,monospace;font-weight:700}
.finding .name{color:var(--muted);font-size:13px}
.finding .stage{margin-left:auto;font-size:12px;color:var(--muted)}
.finding p{margin:6px 0}
.finding .impact{color:var(--muted);font-size:14px}
.pill{display:inline-block;font-size:11px;font-weight:700;letter-spacing:.06em;
  padding:2px 8px;border-radius:999px;color:#fff;text-transform:uppercase}
.pill.error{background:var(--red)} .pill.warn{background:var(--amber)} .pill.info{background:var(--blue)}
.evidence{margin:8px 0 0;padding:0;list-style:none;font-size:13px;color:var(--muted)}
.evidence li{margin:2px 0}
.evidence code{font-family:ui-monospace,Menlo,Consolas,monospace;color:var(--ink)}

/* fix panel */
details.fix{border:1px solid var(--line);border-radius:8px;background:#fff;margin:0 0 12px}
details.fix summary{cursor:pointer;padding:12px 16px;font-weight:600;list-style:none}
details.fix summary::-webkit-details-marker{display:none}
details.fix summary::before{content:"▸ ";color:var(--muted)}
details.fix[open] summary::before{content:"▾ "}
details.fix .body{padding:0 16px 16px;border-top:1px solid var(--line)}
details.fix .body h3{font-size:16px;margin:18px 0 8px}
details.fix .body h4{font-size:14px;margin:14px 0 6px}
details.fix .body pre{background:#0d1117;color:#e6edf3;padding:12px 14px;border-radius:8px;
  overflow-x:auto;font-size:13px}
details.fix .body code{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;
  background:#eff1f3;padding:1px 5px;border-radius:4px;font-size:.88em}
details.fix .body pre code{background:none;padding:0;color:inherit}
details.fix .body table{border-collapse:collapse;margin:10px 0;font-size:14px;width:100%}
details.fix .body th,details.fix .body td{border:1px solid var(--line);padding:6px 10px;text-align:left}
details.fix .body th{background:var(--bg)}
.fix-tools{display:flex;gap:10px;margin-bottom:12px}
.fix-tools button{font:inherit;font-size:13px;padding:6px 12px;border:1px solid var(--line);
  border-radius:6px;background:#fff;cursor:pointer}
.fix-tools button:hover{background:var(--bg)}

/* gates */
table.gates{border-collapse:collapse;width:100%;font-size:14px}
table.gates th,table.gates td{border:1px solid var(--line);padding:8px 12px;text-align:left}
table.gates th{background:var(--bg)}
.pass-tag{color:var(--green);font-weight:700} .fail-tag{color:var(--red);font-weight:700}

.empty{color:var(--muted);font-style:italic}
footer{margin-top:36px;color:var(--muted);font-size:13px;border-top:1px solid var(--line);padding-top:16px}
@media print{.banner{margin:0 0 20px}.fix-tools{display:none}}
</style>
</head>
<body>
<a class="skip" href="#findings">Skip to findings</a>

<div class="banner {{.Verdict}}" role="banner">
  <div class="wrap">
    <h1>ShutdownCheck — <span aria-label="verdict {{.VerdictLabel}}">{{.VerdictLabel}}</span></h1>
    <p>{{.VerdictSummary}}</p>
    <div class="meta">
      target <code>{{.Target}}</code> · profile <code>{{.Profile}}</code>
      {{if .StartedAt}}· started {{.StartedAt}}{{end}} · tool {{.ToolVersion}}
    </div>
  </div>
</div>

<div class="wrap">
  <div class="grid">
    <section class="card" aria-label="Score">
      <h2>Score</h2>
      <div class="gauge">
        <svg width="120" height="120" viewBox="0 0 120 120" role="img"
             aria-label="score {{.Score}} out of 100, grade {{.Grade}}">
          <circle cx="60" cy="60" r="52" fill="none" stroke="#e6e8eb" stroke-width="11"/>
          <circle cx="60" cy="60" r="52" fill="none" stroke="{{.GaugeColor}}" stroke-width="11"
                  stroke-linecap="round" stroke-dasharray="326.73" stroke-dashoffset="{{printf "%.1f" .GaugeDash}}"
                  transform="rotate(-90 60 60)"/>
          <text x="60" y="58" text-anchor="middle" font-size="26" font-weight="700" fill="#1f2328">{{.Score}}</text>
          <text x="60" y="78" text-anchor="middle" font-size="13" fill="#59636e">grade {{.Grade}}</text>
        </svg>
        <div class="num"><strong>{{.Score}}</strong>/100<br>grade {{.Grade}}</div>
      </div>
      <p class="empty" style="font-size:13px">The score is informational — the verdict comes from the rules, not the score.</p>
    </section>

    <section class="card" aria-label="Run timeline">
      <h2>Run timeline <span class="sub">warmup → steady → signal → drain → listener close → exit → kill</span></h2>
      <svg class="tl" viewBox="0 0 {{.AxisW}} 176" role="img"
           aria-label="Timeline of the run with {{len .Markers}} milestones and {{.DotNote}}">
        {{range .Bands}}<rect x="{{printf "%.1f" .X}}" y="4" width="{{printf "%.1f" .Width}}" height="26" rx="4" class="{{.Class}}"/>
        <text x="{{printf "%.1f" .X}}" y="22" dx="8" class="band-label">{{.Label}}</text>{{end}}
        <line x1="8" y1="44" x2="{{printf "%.0f" .AxisEnd}}" y2="44" class="axis"/>
        {{range .Markers}}
        <line x1="{{printf "%.1f" .X}}" y1="34" x2="{{printf "%.1f" .X}}" y2="58" class="{{.Class}}" stroke-width="2">
          <title>{{.Label}} at {{.Sub}}</title>
        </line>
        <text x="{{printf "%.1f" .X}}" y="70" text-anchor="middle" class="mk-label">{{.Label}}</text>
        <text x="{{printf "%.1f" .X}}" y="81" text-anchor="middle" class="mk-sub">{{.Sub}}</text>
        {{end}}
      </svg>
      <h2 style="margin-top:18px">Requests <span class="sub">{{.DotNote}} — hover a dot for details</span></h2>
      <svg class="tl" viewBox="0 0 {{.AxisW}} 172" role="img" aria-label="One dot per request, coloured by outcome">
        {{range .Lanes}}<text x="8" y="{{printf "%.0f" .Y}}" dy="4" class="lane-label">{{.Label}}</text>{{end}}
        {{range .Dots}}<circle cx="{{printf "%.1f" .CX}}" cy="{{printf "%.1f" .CY}}" r="3.4" fill="{{.Fill}}" class="dot"><title>{{.Title}}</title></circle>{{end}}
      </svg>
      <div class="legend" aria-hidden="true">
        <span><i style="background:#2da44e"></i>served</span>
        <span><i style="background:#9a6700"></i>http error</span>
        <span><i style="background:#d97706"></i>refused</span>
        <span><i style="background:#d1242f"></i>destroyed</span>
      </div>
    </section>
  </div>

  <section class="card" id="findings" aria-label="Findings" tabindex="-1">
    <h2>Findings <span class="sub">{{len .Findings}} reported</span></h2>
    {{if .Findings}}
    {{range .Findings}}
    <article class="finding">
      <header>
        <span class="pill {{.Severity}}">{{.Severity}}</span>
        <span class="id">{{.ID}}</span>
        <span class="name">{{.Name}}</span>
        <span class="stage">{{.Stage}}</span>
      </header>
      <p>{{.Summary}}</p>
      {{if .Impact}}<p class="impact">{{.Impact}}</p>{{end}}
      {{if .Evidence}}
      <ul class="evidence">
        {{range .Evidence}}<li><code>{{.Key}}</code>: {{.Value}}</li>{{end}}
      </ul>
      {{end}}
    </article>
    {{end}}
    {{else}}
    <p class="empty">No findings — every check passed.</p>
    {{end}}
  </section>

  {{if .Fixes}}
  <section class="card" aria-label="How to fix">
    <h2>How to fix <span class="sub">broken vs fixed</span></h2>
    <div class="fix-tools">
      <button type="button" id="expandFixes">Expand all</button>
      <button type="button" id="collapseFixes">Collapse all</button>
    </div>
    {{range .Fixes}}
    <details class="fix"{{if .Open}} open{{end}}>
      <summary><span class="pill {{.Severity}}">{{.Severity}}</span> {{.ID}} — {{.Name}}</summary>
      <div class="body">{{.Body}}</div>
    </details>
    {{end}}
  </section>
  {{end}}

  {{if .HasGates}}
  <section class="card" aria-label="Gates">
    <h2>Gates</h2>
    <table class="gates">
      <thead><tr><th>Gate</th><th>Threshold</th><th>Actual</th><th>Result</th></tr></thead>
      <tbody>
        {{range .Gates}}<tr>
          <td>{{.Name}}</td><td>{{.Threshold}}</td><td>{{printf "%.3g" .Actual}}</td>
          <td>{{if .Passed}}<span class="pass-tag">pass</span>{{else}}<span class="fail-tag">breached</span>{{end}}</td>
        </tr>{{end}}
      </tbody>
    </table>
  </section>
  {{end}}

  <footer>
    ShutdownCheck {{.ToolVersion}} · report schema {{.SchemaVersion}} · self-contained — no external requests.
  </footer>
</div>

<script>
(function(){
  var fixes = document.querySelectorAll('details.fix');
  var ex = document.getElementById('expandFixes');
  var co = document.getElementById('collapseFixes');
  if(ex) ex.addEventListener('click', function(){ fixes.forEach(function(d){ d.open = true; }); });
  if(co) co.addEventListener('click', function(){ fixes.forEach(function(d){ d.open = false; }); });
})();
</script>
</body>
</html>`))
