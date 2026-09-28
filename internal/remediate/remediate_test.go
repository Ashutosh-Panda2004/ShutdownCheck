package remediate

import (
	"strings"
	"testing"

	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/analyze"
)

// A finding the tool cannot explain is a finding nobody can act on, so every
// registered signature must resolve to something useful even before a page has
// been hand-written for it.
func TestEverySignatureHasAnExplanation(t *testing.T) {
	for _, info := range analyze.Catalog() {
		page, err := Explain(info.ID)
		if err != nil {
			t.Errorf("Explain(%s): %v", info.ID, err)
			continue
		}

		for _, want := range []string{string(info.ID), info.Name} {
			if !strings.Contains(page, want) {
				t.Errorf("%s page does not mention %q", info.ID, want)
			}
		}
		if !strings.Contains(strings.ToLower(page), "fix") {
			t.Errorf("%s page does not say how to fix anything", info.ID)
		}
		if len(page) < 200 {
			t.Errorf("%s page is only %d bytes; too thin to be useful", info.ID, len(page))
		}
	}
}

func TestExplainRejectsUnknownSignature(t *testing.T) {
	if _, err := Explain("SC999"); err == nil {
		t.Fatal("an unknown signature should be an error, not an empty page")
	}
}

func TestHandWrittenPagesAreUsed(t *testing.T) {
	written := WrittenPages()
	if len(written) == 0 {
		t.Fatal("no hand-written pages are embedded")
	}

	for _, id := range written {
		if !HasWrittenPage(analyze.SignatureID(id)) {
			t.Errorf("%s is listed but not found", id)
		}

		page, err := Explain(analyze.SignatureID(id))
		if err != nil {
			t.Fatalf("Explain(%s): %v", id, err)
		}
		// Generated pages carry this line; a hand-written one must not.
		if strings.Contains(page, "has not been written yet") {
			t.Errorf("%s resolved to a generated page despite having a written one", id)
		}
	}
}

func TestGeneratedPagesAreMarkedAsSuch(t *testing.T) {
	var generated analyze.SignatureID
	for _, info := range analyze.Catalog() {
		if !HasWrittenPage(info.ID) {
			generated = info.ID
			break
		}
	}
	if generated == "" {
		t.Skip("every signature now has a hand-written page")
	}

	page, err := Explain(generated)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !strings.Contains(page, "has not been written yet") {
		t.Error("a generated page should say so, and invite a contribution")
	}
}

func TestDetectStack(t *testing.T) {
	cases := map[string]struct {
		hints Hints
		want  Stack
	}{
		"explicit override wins": {
			Hints{Override: "rust-axum", ServerHeader: "Kestrel"}, StackRustAxum,
		},
		"invalid override is ignored": {
			Hints{Override: "not-a-stack", ServerHeader: "Kestrel"}, StackDotNet,
		},
		"kestrel header":   {Hints{ServerHeader: "Kestrel"}, StackDotNet},
		"gunicorn header":  {Hints{ServerHeader: "gunicorn/21.2.0"}, StackPythonGunicorn},
		"uvicorn header":   {Hints{ServerHeader: "uvicorn"}, StackPythonUvicorn},
		"puma header":      {Hints{ServerHeader: "puma 6"}, StackRubyPuma},
		"nginx header":     {Hints{ServerHeader: "nginx/1.25"}, StackNginx},
		"powered by":       {Hints{PoweredBy: "Express"}, StackNodeExpress},
		"image name":       {Hints{Image: "registry/my-spring-app:1.2"}, StackJavaSpring},
		"argv jar":         {Hints{Argv: []string{"java", "-jar", "app.jar"}}, StackJavaSpring},
		"argv dotnet":      {Hints{Argv: []string{"dotnet", "Api.dll"}}, StackDotNet},
		"argv gunicorn":    {Hints{Argv: []string{"gunicorn", "app:application"}}, StackPythonGunicorn},
		"argv node":        {Hints{Argv: []string{"node", "server.js"}}, StackNodeExpress},
		"nothing to go on": {Hints{}, StackUnknown},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := DetectStack(tc.hints); got != tc.want {
				t.Errorf("DetectStack(%+v) = %q, want %q", tc.hints, got, tc.want)
			}
		})
	}
}

// Detection only chooses better wording for advice, so a wrong guess must never
// be able to change a verdict. The header wins over the launch command because
// it is what the server said about itself.
func TestServerHeaderOutranksLaunchCommand(t *testing.T) {
	got := DetectStack(Hints{ServerHeader: "Kestrel", Argv: []string{"node", "server.js"}})
	if got != StackDotNet {
		t.Fatalf("DetectStack = %q, want the header to win", got)
	}
}

func TestStackValidity(t *testing.T) {
	for _, s := range Stacks() {
		if !s.Valid() {
			t.Errorf("%q is listed but reports itself invalid", s)
		}
	}
	if !StackUnknown.Valid() {
		t.Error("an unknown stack is a legitimate state")
	}
	if Stack("cobol-cics").Valid() {
		t.Error("an unrecognised stack must not validate")
	}
}
