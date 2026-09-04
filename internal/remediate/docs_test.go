package remediate_test

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shutdowncheck/shutdowncheck/internal/analyze"
	"github.com/shutdowncheck/shutdowncheck/internal/remediate"
)

var update = flag.Bool("update", false, "rewrite the published signature pages under docs/signatures")

// docsDir is the published documentation, one page per signature.
//
// The pages are generated rather than hand-maintained alongside the catalogue,
// because a documentation set that drifts from the tool is worse than none: a
// user searching for a signature they were just shown must not land on a page
// describing something else.
const docsDir = "../../docs/signatures"

func TestPublishedSignaturePagesAreCurrent(t *testing.T) {
	for _, info := range analyze.Catalog() {
		want, err := remediate.Explain(info.ID)
		if err != nil {
			t.Fatalf("Explain(%s): %v", info.ID, err)
		}
		want = withFrontMatter(info, want)

		path := filepath.Join(docsDir, string(info.ID)+".md")

		if *update {
			writeFile(t, path, want)
			continue
		}

		got, err := os.ReadFile(path) // #nosec G304 -- fixed path built from the catalogue
		if err != nil {
			t.Errorf("%s has no published page: %v\nrun: go test ./internal/remediate -update", info.ID, err)
			continue
		}
		if normalise(string(got)) != normalise(want) {
			t.Errorf("%s is out of date\nrun: go test ./internal/remediate -update", path)
		}
	}
}

func TestSignatureIndexIsCurrent(t *testing.T) {
	want := buildIndex()
	path := filepath.Join(docsDir, "README.md")

	if *update {
		writeFile(t, path, want)
		return
	}

	got, err := os.ReadFile(path) // #nosec G304 -- fixed path
	if err != nil {
		t.Fatalf("no signature index: %v\nrun: go test ./internal/remediate -update", err)
	}
	if normalise(string(got)) != normalise(want) {
		t.Errorf("%s is out of date\nrun: go test ./internal/remediate -update", path)
	}
}

// The published pages must not contain anything the binary cannot also print
// offline, or `explain` and the website would answer the same question
// differently.
func TestEverySignatureIsExplainable(t *testing.T) {
	for _, info := range analyze.Catalog() {
		page, err := remediate.Explain(info.ID)
		if err != nil {
			t.Errorf("%s cannot be explained: %v", info.ID, err)
			continue
		}
		if !strings.Contains(page, string(info.ID)) {
			t.Errorf("the page for %s never names it", info.ID)
		}
	}
}

// withFrontMatter adds the metadata a static site generator needs. The
// description is what search engines show, which is the entire point of
// publishing these separately from the binary.
func withFrontMatter(info analyze.SignatureInfo, body string) string {
	var b strings.Builder

	b.WriteString("---\n")
	fmt.Fprintf(&b, "title: %q\n", fmt.Sprintf("%s — %s", info.ID, info.Name))
	fmt.Fprintf(&b, "description: %q\n", info.Summary)
	fmt.Fprintf(&b, "signature: %s\n", info.ID)
	if info.Stage != analyze.StageNone {
		fmt.Fprintf(&b, "stage: %s\n", info.Stage)
	}
	b.WriteString("---\n\n")
	b.WriteString(body)

	if !strings.HasSuffix(body, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("\n---\n\n")
	fmt.Fprintf(&b, "Reproduce this locally:\n\n```console\n$ shutdowncheck explain %s\n```\n", info.ID)

	return b.String()
}

func buildIndex() string {
	var b strings.Builder

	b.WriteString(`# Failure signatures

Every finding ShutdownCheck reports maps to exactly one signature below, and
every signature maps to one of the seven stages of correct termination.

These pages are generated from the catalogue compiled into the binary, so they
cannot drift from what the tool actually reports. Run ` + "`go test ./internal/remediate -update`" + ` after
changing the catalogue.

The same text is available offline:

` + "```console\n$ shutdowncheck explain SC006\n```" + `

| ID | Stage | Name | What it means |
| --- | --- | --- | --- |
`)

	for _, info := range analyze.Catalog() {
		stage := string(info.Stage)
		if stage == "" {
			stage = "—"
		}
		fmt.Fprintf(&b, "| [%s](%s.md) | %s | %s | %s |\n",
			info.ID, info.ID, stage, info.Name, escapePipes(info.Summary))
	}

	b.WriteString(`
## The seven stages

| | Stage | Must happen |
| --- | --- | --- |
| S1 | Signal received | Handle ` + "`SIGTERM`" + `; don't ignore it |
| S2 | Readiness flipped | Health endpoint starts failing immediately |
| S3 | Lame-duck window | Keep serving new connections while de-registration propagates |
| S4 | Listener closed | Stop accepting once the window elapses |
| S5 | Connection close signalled | ` + "`Connection: close`" + ` / HTTP-2 ` + "`GOAWAY`" + ` |
| S6 | In-flight work drained | Every active request completes |
| S7 | Clean exit inside budget | Exit before the grace period expires, no orphans |
`)

	return b.String()
}

func escapePipes(s string) string { return strings.ReplaceAll(s, "|", `\|`) }

// normalise makes the comparison indifferent to the line endings git may have
// checked the files out with.
func normalise(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), "\r\n", "\n")
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
