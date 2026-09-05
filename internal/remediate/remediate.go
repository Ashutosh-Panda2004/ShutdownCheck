package remediate

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/shutdowncheck/shutdowncheck/internal/analyze"
)

//go:embed signatures/*.md
var content embed.FS

// Stack identifies the target's framework, so advice can be specific rather
// than generic.
type Stack string

// StackUnknown and the other stack constants identify supported remediation
// targets.
const (
	StackUnknown        Stack = ""
	StackGoNetHTTP      Stack = "go-net-http"
	StackNodeExpress    Stack = "node-express"
	StackNodeFastify    Stack = "node-fastify"
	StackPythonGunicorn Stack = "python-gunicorn"
	StackPythonUvicorn  Stack = "python-uvicorn"
	StackJavaSpring     Stack = "java-spring-boot"
	StackDotNet         Stack = "dotnet-aspnetcore"
	StackRubyPuma       Stack = "ruby-puma"
	StackRustAxum       Stack = "rust-axum"
	StackNginx          Stack = "nginx"
)

// Stacks lists the stacks the tool can name, for help text and validation.
func Stacks() []Stack {
	return []Stack{
		StackGoNetHTTP, StackNodeExpress, StackNodeFastify,
		StackPythonGunicorn, StackPythonUvicorn, StackJavaSpring,
		StackDotNet, StackRubyPuma, StackRustAxum, StackNginx,
	}
}

// Valid reports whether s is a recognised stack.
func (s Stack) Valid() bool {
	if s == StackUnknown {
		return true
	}
	for _, known := range Stacks() {
		if s == known {
			return true
		}
	}
	return false
}

// Hints are the observations stack detection can draw on.
type Hints struct {
	Override     string
	ServerHeader string
	PoweredBy    string
	Image        string
	Argv         []string
}

// DetectStack identifies the framework behind the target.
//
// Sources are consulted in order of how much they can be trusted: an explicit
// flag first, then what the server said about itself, then how it was launched.
// Detection only ever selects better wording for advice, so a wrong guess costs
// nothing and is never allowed to influence a verdict.
func DetectStack(h Hints) Stack {
	if h.Override != "" {
		if s := Stack(h.Override); s.Valid() && s != StackUnknown {
			return s
		}
	}

	if s := fromServerHeader(h.ServerHeader); s != StackUnknown {
		return s
	}
	if s := fromPoweredBy(h.PoweredBy); s != StackUnknown {
		return s
	}
	if s := fromWords(h.Image); s != StackUnknown {
		return s
	}
	return fromWords(strings.Join(h.Argv, " "))
}

func fromServerHeader(header string) Stack {
	switch h := strings.ToLower(header); {
	case h == "":
		return StackUnknown
	case strings.Contains(h, "kestrel"):
		return StackDotNet
	case strings.Contains(h, "gunicorn"):
		return StackPythonGunicorn
	case strings.Contains(h, "uvicorn"):
		return StackPythonUvicorn
	case strings.Contains(h, "puma"):
		return StackRubyPuma
	case strings.Contains(h, "nginx"):
		return StackNginx
	default:
		return StackUnknown
	}
}

func fromPoweredBy(header string) Stack {
	if strings.Contains(strings.ToLower(header), "express") {
		return StackNodeExpress
	}
	return StackUnknown
}

func fromWords(s string) Stack {
	lower := strings.ToLower(s)
	if lower == "" {
		return StackUnknown
	}

	// Ordered so that more specific markers win over the runtime that hosts them.
	for _, candidate := range []struct {
		needles []string
		stack   Stack
	}{
		{[]string{"gunicorn"}, StackPythonGunicorn},
		{[]string{"uvicorn"}, StackPythonUvicorn},
		{[]string{"fastify"}, StackNodeFastify},
		{[]string{"express"}, StackNodeExpress},
		{[]string{"spring", "java", ".jar"}, StackJavaSpring},
		{[]string{"dotnet", ".dll"}, StackDotNet},
		{[]string{"puma", "rails"}, StackRubyPuma},
		{[]string{"nginx"}, StackNginx},
		{[]string{"node"}, StackNodeExpress},
	} {
		for _, needle := range candidate.needles {
			if strings.Contains(lower, needle) {
				return candidate.stack
			}
		}
	}
	return StackUnknown
}

// Explain returns the documentation for a signature.
//
// Every registered signature always has an explanation: where no page has been
// written yet, one is generated from the catalogue. A finding the tool cannot
// explain is a finding a user cannot act on, so "no docs yet" is not an
// acceptable answer.
func Explain(id analyze.SignatureID) (string, error) {
	info, ok := analyze.Lookup(id)
	if !ok {
		return "", fmt.Errorf("unknown signature %q; run `shutdowncheck explain` to list them all", id)
	}

	if page, err := content.ReadFile("signatures/" + string(id) + ".md"); err == nil {
		return string(page), nil
	}
	return generatePage(info), nil
}

// HasWrittenPage reports whether a hand-written page exists, which the docs
// build uses to track coverage.
func HasWrittenPage(id analyze.SignatureID) bool {
	_, err := content.ReadFile("signatures/" + string(id) + ".md")
	return err == nil
}

// WrittenPages lists the signatures with hand-written documentation.
func WrittenPages() []string {
	entries, err := fs.ReadDir(content, "signatures")
	if err != nil {
		return nil
	}

	var out []string
	for _, entry := range entries {
		out = append(out, strings.TrimSuffix(entry.Name(), ".md"))
	}
	sort.Strings(out)
	return out
}

func generatePage(info analyze.SignatureInfo) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# %s — %s\n\n", info.ID, info.Name)
	fmt.Fprintf(&b, "**Stage %s of the seven stages of correct termination.**\n\n", info.Stage)
	fmt.Fprintf(&b, "## What was observed\n\n%s\n\n", info.Summary)
	fmt.Fprintf(&b, "## Why it matters\n\n%s\n\n", info.Impact)
	b.WriteString("## How to fix it\n\n")
	b.WriteString(stageGuidance(info.Stage))
	fmt.Fprintf(&b, "\nA stack-specific page for %s has not been written yet. "+
		"If you work out the fix for your framework, a pull request adding it would be welcome.\n", info.ID)

	return b.String()
}

// stageGuidance is the advice that holds for any framework at a given stage.
func stageGuidance(stage analyze.Stage) string {
	switch stage {
	case analyze.StageSignalReceived:
		return "Install a handler for `SIGTERM` and make it start your shutdown sequence. " +
			"A process with no handler is killed outright, destroying whatever it was doing.\n"
	case analyze.StageReadinessFlip:
		return "Make your readiness endpoint start failing the moment the signal arrives, before anything else. " +
			"That is what tells the load balancer to stop sending you new work.\n"
	case analyze.StageLameDuck:
		return "Keep serving new connections for a few seconds after the signal. " +
			"Removing an instance from a load balancer is asynchronous, so traffic keeps arriving after you are told to stop.\n"
	case analyze.StageListenerClosed:
		return "Once the de-registration window has elapsed, stop accepting new connections while leaving established ones open.\n"
	case analyze.StageConnClose:
		return "Tell clients the connection is going away: send `Connection: close` on HTTP/1.1 responses, " +
			"or a `GOAWAY` frame on HTTP/2, so they stop reusing a socket that is about to die.\n"
	case analyze.StageDrain:
		return "Wait for in-flight requests to finish before exiting, and flush any background work " +
			"such as queue consumers or buffered telemetry.\n"
	case analyze.StageCleanExit:
		return "Exit cleanly and comfortably inside the grace period, making sure nothing you spawned outlives you.\n"
	default:
		return "See the seven stages of correct termination in the project documentation.\n"
	}
}
