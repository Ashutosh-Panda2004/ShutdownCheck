package probe

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

func TestRedactHeaders(t *testing.T) {
	in := http.Header{
		"Authorization": []string{"Bearer supersecret"},
		"Cookie":        []string{"session=abc"},
		"Content-Type":  []string{"application/json"},
		"X-Request-Id":  []string{"req-1", "req-2"},
	}

	got := RedactHeaders(in)

	if v := got.Get("Authorization"); v != Redacted {
		t.Errorf("Authorization = %q, want %q", v, Redacted)
	}
	if v := got.Get("Cookie"); v != Redacted {
		t.Errorf("Cookie = %q, want %q", v, Redacted)
	}
	if v := got.Get("Content-Type"); v != "application/json" {
		t.Errorf("Content-Type = %q, want it preserved", v)
	}
	if len(got["X-Request-Id"]) != 2 {
		t.Errorf("multi-valued headers should be preserved, got %v", got["X-Request-Id"])
	}

	// The original must not be modified; callers still need to send the real value.
	if in.Get("Authorization") != "Bearer supersecret" {
		t.Error("RedactHeaders mutated its input")
	}

	if RedactHeaders(nil) != nil {
		t.Error("RedactHeaders(nil) should return nil")
	}
}

func TestRedactURL(t *testing.T) {
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
			got := RedactURL(tc.in)
			for _, secret := range tc.mustHide {
				if strings.Contains(got, secret) {
					t.Errorf("RedactURL(%q) = %q, still contains %q", tc.in, got, secret)
				}
			}
			for _, keep := range tc.mustKeep {
				if !strings.Contains(got, keep) {
					t.Errorf("RedactURL(%q) = %q, lost %q", tc.in, got, keep)
				}
			}
		})
	}

	if got := RedactURL(""); got != "" {
		t.Errorf("RedactURL(\"\") = %q, want empty", got)
	}
}

// An unparseable URL cannot be shown to be safe, so it must not be passed
// through on the assumption that it is harmless.
func TestRedactURLFailsClosed(t *testing.T) {
	if got := RedactURL("http://[::1]:namedport/x?token=secret"); strings.Contains(got, "secret") {
		t.Fatalf("an unparseable URL leaked its query: %q", got)
	}
}

func TestRedactMessage(t *testing.T) {
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
			got := RedactMessage(tc.in)
			if tc.mustHide != "" && strings.Contains(got, tc.mustHide) {
				t.Errorf("RedactMessage(%q) = %q, still contains %q", tc.in, got, tc.mustHide)
			}
			if tc.mustKeep != "" && !strings.Contains(got, tc.mustKeep) {
				t.Errorf("RedactMessage(%q) = %q, lost %q", tc.in, got, tc.mustKeep)
			}
		})
	}

	if got := RedactMessage(""); got != "" {
		t.Errorf("RedactMessage(\"\") = %q, want empty", got)
	}
}

func TestRedactMessageHandlesMultipleURLs(t *testing.T) {
	in := `first "http://a/?token=aaa" then "https://b/?secret=bbb" done`

	got := RedactMessage(in)
	for _, secret := range []string{"aaa", "bbb"} {
		if strings.Contains(got, secret) {
			t.Errorf("RedactMessage leaked %q: %q", secret, got)
		}
	}
	if !strings.Contains(got, "done") {
		t.Errorf("trailing text was lost: %q", got)
	}
}

func TestRemoteAddrHandlesNil(t *testing.T) {
	if got := remoteAddr(nil); got != "" {
		t.Errorf("remoteAddr(nil) = %q, want empty", got)
	}
}

func TestTrackedFromRejectsUnknownConn(t *testing.T) {
	if _, ok := trackedFrom(nil); ok {
		t.Error("trackedFrom(nil) should not report success")
	}
}
