package redact

import (
	"net/http"
	"strings"
	"testing"
)

func TestIsSensitiveHeader(t *testing.T) {
	sensitive := []string{
		"Authorization", "authorization", "  Authorization  ",
		"Proxy-Authorization", "Cookie", "Set-Cookie", "X-Api-Key", "X-Auth-Token", "X-CSRF-Token",
	}
	for _, name := range sensitive {
		if !IsSensitiveHeader(name) {
			t.Errorf("IsSensitiveHeader(%q) = false, want true", name)
		}
	}

	for _, name := range []string{"Content-Type", "Accept", "X-Request-Id", ""} {
		if IsSensitiveHeader(name) {
			t.Errorf("IsSensitiveHeader(%q) = true, want false", name)
		}
	}
}

func TestHeaders(t *testing.T) {
	in := http.Header{
		"Authorization": []string{"Bearer supersecret"},
		"Cookie":        []string{"session=abc"},
		"Content-Type":  []string{"application/json"},
		"X-Request-Id":  []string{"req-1", "req-2"},
	}

	got := Headers(in)

	if v := got.Get("Authorization"); v != Placeholder {
		t.Errorf("Authorization = %q, want %q", v, Placeholder)
	}
	if v := got.Get("Cookie"); v != Placeholder {
		t.Errorf("Cookie = %q, want %q", v, Placeholder)
	}
	if v := got.Get("Content-Type"); v != "application/json" {
		t.Errorf("Content-Type = %q, want it preserved", v)
	}
	if len(got["X-Request-Id"]) != 2 {
		t.Errorf("multi-valued headers should be preserved, got %v", got["X-Request-Id"])
	}

	// The original must not be modified; callers still need to send the real value.
	if in.Get("Authorization") != "Bearer supersecret" {
		t.Error("Headers mutated its input")
	}

	if Headers(nil) != nil {
		t.Error("Headers(nil) should return nil")
	}
}

func TestURL(t *testing.T) {
	cases := map[string]struct {
		in       string
		mustHide []string
		mustKeep []string
	}{
		"token in query": {
			in:       "http://host/path?token=supersecret&page=2",
			mustHide: []string{"supersecret"},
			mustKeep: []string{"host", "path", "page=2"},
		},
		"api key": {
			in:       "https://host/x?api_key=abc123",
			mustHide: []string{"abc123"},
		},
		"mixed case key": {
			in:       "https://host/x?ToKeN=abc123",
			mustHide: []string{"abc123"},
		},
		"basic auth userinfo": {
			in:       "https://user:hunter2@host/x",
			mustHide: []string{"hunter2"},
		},
		"nothing sensitive": {
			in:       "http://host/path?page=2",
			mustKeep: []string{"page=2", "host"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := URL(tc.in)
			for _, secret := range tc.mustHide {
				if strings.Contains(got, secret) {
					t.Errorf("URL(%q) = %q, still contains %q", tc.in, got, secret)
				}
			}
			for _, keep := range tc.mustKeep {
				if !strings.Contains(got, keep) {
					t.Errorf("URL(%q) = %q, lost %q", tc.in, got, keep)
				}
			}
		})
	}

	if got := URL(""); got != "" {
		t.Errorf("URL(\"\") = %q, want empty", got)
	}
}

// An unparseable URL cannot be shown to be safe, so it must not be passed
// through on the assumption that it is harmless.
func TestURLFailsClosed(t *testing.T) {
	if got := URL("http://[::1]:namedport/x?token=secret"); strings.Contains(got, "secret") {
		t.Fatalf("an unparseable URL leaked its query: %q", got)
	}
}

func TestMessage(t *testing.T) {
	cases := map[string]struct {
		in       string
		mustHide string
		mustKeep string
	}{
		"quoted url": {
			in:       `Get "http://host/x?token=secret": dial tcp 1.2.3.4:80: connect: connection refused`,
			mustHide: "secret",
			mustKeep: "connection refused",
		},
		"no url": {
			in:       "connection reset by peer",
			mustKeep: "connection reset by peer",
		},
		"unbalanced quote": {
			in:       `Get "http://host/x?token=secret`,
			mustKeep: "Get",
		},
		"quoted non-url": {
			in:       `parsing "not a url": bad`,
			mustKeep: "not a url",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := Message(tc.in)
			if tc.mustHide != "" && strings.Contains(got, tc.mustHide) {
				t.Errorf("Message(%q) = %q, still contains %q", tc.in, got, tc.mustHide)
			}
			if tc.mustKeep != "" && !strings.Contains(got, tc.mustKeep) {
				t.Errorf("Message(%q) = %q, lost %q", tc.in, got, tc.mustKeep)
			}
		})
	}

	if got := Message(""); got != "" {
		t.Errorf("Message(\"\") = %q, want empty", got)
	}
}

func TestMessageHandlesMultipleURLs(t *testing.T) {
	in := `first "http://a/?token=aaa" then "https://b/?secret=bbb" done`

	got := Message(in)
	for _, secret := range []string{"aaa", "bbb"} {
		if strings.Contains(got, secret) {
			t.Errorf("Message leaked %q: %q", secret, got)
		}
	}
	if !strings.Contains(got, "done") {
		t.Errorf("trailing text was lost: %q", got)
	}
}

func TestNonSensitiveHeadersSurviveUnchanged(t *testing.T) {
	in := http.Header{"Accept": []string{"application/json"}}

	if got := Headers(in).Get("Accept"); got != "application/json" {
		t.Errorf("Accept = %q, want it preserved", got)
	}
}
