package report

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/shutdowncheck/shutdowncheck/internal/analyze"
	"github.com/shutdowncheck/shutdowncheck/internal/timeline"
	"github.com/shutdowncheck/shutdowncheck/pkg/schema"
)

// Format names the output rendering.
type Format string

const (
	FormatHuman    Format = "human"
	FormatJSON     Format = "json"
	FormatJUnit    Format = "junit"
	FormatMarkdown Format = "markdown"
	FormatNDJSON   Format = "ndjson"
)

// Formats lists every supported format, for validation and help text.
func Formats() []Format {
	return []Format{FormatHuman, FormatJSON, FormatJUnit, FormatMarkdown, FormatNDJSON}
}

// Valid reports whether f is a known format.
func (f Format) Valid() bool {
	for _, known := range Formats() {
		if f == known {
			return true
		}
	}
	return false
}

// JSON writes the versioned report.
func JSON(w io.Writer, report schema.Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

// NDJSON writes the raw evidence stream, so a run can be re-analysed later,
// including under a different shutdown profile.
func NDJSON(w io.Writer, tl timeline.Timeline) error {
	return timeline.WriteNDJSON(w, tl)
}

type junitSuites struct {
	XMLName  xml.Name     `xml:"testsuites"`
	Name     string       `xml:"name,attr"`
	Tests    int          `xml:"tests,attr"`
	Failures int          `xml:"failures,attr"`
	Suites   []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name     string      `xml:"name,attr"`
	Tests    int         `xml:"tests,attr"`
	Failures int         `xml:"failures,attr"`
	Cases    []junitCase `xml:"testcase"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	ClassName string        `xml:"classname,attr"`
	Failure   *junitFailure `xml:"failure,omitempty"`
}

type junitFailure struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Text    string `xml:",chardata"`
}

// JUnit writes results as a test report.
//
// Every signature becomes a test case, not just the ones that fired, so a CI
// run shows what was actually checked. A gate that a user configured is a case
// too, since "the threshold I set was not met" is a distinct failure from "a
// defect was detected".
func JUnit(w io.Writer, result analyze.Result) error {
	fired := map[string]schema.Finding{}
	for _, finding := range result.Report.Findings {
		fired[finding.ID] = finding
	}

	suite := junitSuite{Name: "shutdowncheck"}

	for _, info := range analyze.Catalog() {
		id := string(info.ID)
		c := junitCase{Name: id + " " + info.Name, ClassName: "shutdowncheck.signatures"}

		if finding, ok := fired[id]; ok && finding.Severity == schema.SeverityError {
			c.Failure = &junitFailure{
				Message: finding.Summary,
				Type:    info.Name,
				Text:    finding.Summary + "\n\n" + finding.Impact + "\n\n" + evidenceText(finding.Evidence),
			}
			suite.Failures++
		}
		suite.Cases = append(suite.Cases, c)
		suite.Tests++
	}

	for _, gate := range result.Report.Gates {
		c := junitCase{Name: "gate " + gate.Name, ClassName: "shutdowncheck.gates"}
		if !gate.Passed {
			c.Failure = &junitFailure{
				Message: fmt.Sprintf("%.4g exceeds the configured limit of %.4g", gate.Actual, gate.Threshold),
				Type:    "gate",
			}
			suite.Failures++
		}
		suite.Cases = append(suite.Cases, c)
		suite.Tests++
	}

	doc := junitSuites{
		Name:     "shutdowncheck",
		Tests:    suite.Tests,
		Failures: suite.Failures,
		Suites:   []junitSuite{suite},
	}

	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n")
	return err
}

// Markdown writes a summary for a pull request comment or a CI job summary.
func Markdown(w io.Writer, result analyze.Result) error {
	rep := result.Report
	var b strings.Builder

	icon := map[schema.Verdict]string{
		schema.VerdictPass:         "PASS",
		schema.VerdictFail:         "FAIL",
		schema.VerdictInconclusive: "INCONCLUSIVE",
	}[rep.Verdict]

	fmt.Fprintf(&b, "## ShutdownCheck: %s\n\n", icon)
	fmt.Fprintf(&b, "**Score %d/100 (%s)** &middot; profile `%s`", rep.Score.Value, rep.Score.Grade, rep.Run.Profile)
	if rep.Target.Label != "" {
		fmt.Fprintf(&b, " &middot; target `%s`", rep.Target.Label)
	}
	if rep.Run.Trials.Total > 1 {
		fmt.Fprintf(&b, " &middot; %d of %d trials failed", rep.Run.Trials.Failed, rep.Run.Trials.Total)
		if !rep.Run.Trials.Consistent {
			b.WriteString(" (flaky)")
		}
	}
	b.WriteString("\n\n")

	if rep.Verdict == schema.VerdictInconclusive {
		b.WriteString("> This run did not produce enough evidence to judge shutdown behaviour. " +
			"It is **not** a pass.\n\n")
	}

	b.WriteString("| Phase | Requests | OK | Failed |\n|---|---:|---:|---:|\n")
	for _, row := range []struct {
		label string
		stats schema.PhaseStats
	}{
		{"Before the signal", rep.Requests.ByPhase.Steady},
		{"In flight at signal", rep.Requests.ByPhase.InFlight},
		{"After the signal", rep.Requests.ByPhase.PostSignal},
		{"Past the window", rep.Requests.ByPhase.PostWindow},
	} {
		if row.stats.Count == 0 {
			continue
		}
		fmt.Fprintf(&b, "| %s | %d | %d | %d |\n", row.label, row.stats.Count, row.stats.OK, row.stats.Failed)
	}
	b.WriteString("\n")

	if len(rep.Findings) == 0 {
		b.WriteString("No findings: this service terminates correctly.\n")
		_, err := io.WriteString(w, b.String())
		return err
	}

	b.WriteString("### Findings\n\n| | ID | Finding | Detail |\n|---|---|---|---|\n")
	for _, finding := range rep.Findings {
		severity := "warn"
		if finding.Severity == schema.SeverityError {
			severity = "**error**"
		}
		fmt.Fprintf(&b, "| %s | [`%s`](%s) | %s | %s |\n",
			severity, finding.ID, finding.DocsURL, finding.Name, finding.Summary)
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// Badge writes an SVG score badge for a README.
func Badge(w io.Writer, report schema.Report) error {
	colour := map[string]string{
		"A": "#4c1", "B": "#97ca00", "C": "#dfb317", "D": "#fe7d37", "F": "#e05d44",
	}[report.Score.Grade]
	if colour == "" {
		colour = "#9f9f9f"
	}

	value := fmt.Sprintf("%d/100 (%s)", report.Score.Value, report.Score.Grade)
	const labelWidth = 96
	valueWidth := 8*len(value) + 20
	total := labelWidth + valueWidth

	svg := fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="20" role="img" aria-label="shutdowncheck: %s">
  <title>shutdowncheck: %s</title>
  <linearGradient id="s" x2="0" y2="100%%"><stop offset="0" stop-color="#bbb" stop-opacity=".1"/><stop offset="1" stop-opacity=".1"/></linearGradient>
  <clipPath id="r"><rect width="%d" height="20" rx="3" fill="#fff"/></clipPath>
  <g clip-path="url(#r)">
    <rect width="%d" height="20" fill="#555"/>
    <rect x="%d" width="%d" height="20" fill="%s"/>
    <rect width="%d" height="20" fill="url(#s)"/>
  </g>
  <g fill="#fff" text-anchor="middle" font-family="Verdana,Geneva,DejaVu Sans,sans-serif" font-size="11">
    <text x="%d" y="14">shutdowncheck</text>
    <text x="%d" y="14">%s</text>
  </g>
</svg>
`, total, value, value, total, labelWidth, labelWidth, valueWidth, colour, total,
		labelWidth/2, labelWidth+valueWidth/2, value)

	_, err := io.WriteString(w, svg)
	return err
}

// evidenceText renders evidence deterministically for embedding in XML.
func evidenceText(evidence map[string]any) string {
	if len(evidence) == 0 {
		return ""
	}

	keys := make([]string, 0, len(evidence))
	for k := range evidence {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, evidence[k]))
	}
	return strings.Join(parts, " ")
}
