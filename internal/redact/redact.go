// Package redact removes values that must never reach a report, a log line or
// the NDJSON stream.
//
// It is its own package because probing, target log capture and reporting all
// need it. Secret handling is exactly the kind of rule that decays when each
// caller reimplements it, so there is one implementation and one set of tests.
package redact

import (
	"net/http"
	"net/url"
	"strings"
)

// Placeholder replaces any redacted value.
const Placeholder = "REDACTED"

// sensitiveHeaders are removed from anything the tool records. ShutdownCheck is
// meant to run in CI against services that need credentials, so redaction is a
// correctness requirement rather than a courtesy.
var sensitiveHeaders = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"cookie":              true,
	"set-cookie":          true,
	"x-api-key":           true,
	"x-auth-token":        true,
	"x-csrf-token":        true,
}

// sensitiveQueryKeys are redacted from URLs. Error strings returned by net/http
// embed the request URL, so a token in a query string would otherwise leak into
// a report through the error detail alone.
var sensitiveQueryKeys = map[string]bool{
	"access_token": true,
	"api_key":      true,
	"apikey":       true,
	"auth":         true,
	"key":          true,
	"password":     true,
	"passwd":       true,
	"pwd":          true,
	"secret":       true,
	"sig":          true,
	"signature":    true,
	"token":        true,
}

// IsSensitiveHeader reports whether a header must be redacted.
func IsSensitiveHeader(name string) bool {
	return sensitiveHeaders[strings.ToLower(strings.TrimSpace(name))]
}

// sensitiveWords mark a flag or variable whose value must not be recorded.
// Matched as substrings, so --db-password and DATABASE_TOKEN are both caught.
var sensitiveWords = []string{
	"apikey", "api-key", "api_key",
	"auth", "credential", "passwd", "password",
	"private", "pwd", "secret", "token",
}

func isSensitiveName(name string) bool {
	lower := strings.ToLower(strings.TrimLeft(name, "-"))
	for _, word := range sensitiveWords {
		if strings.Contains(lower, word) {
			return true
		}
	}
	return false
}

// Argv renders a command line with secret-looking values removed.
//
// A target's argv is echoed back in the report so a reader knows what was run,
// and `./api --db-password=hunter2` is an ordinary way to start a service. The
// report is an artefact that gets uploaded to CI and shared, which is a longer
// and wider exposure than the process table it came from.
func Argv(argv []string) string {
	out := make([]string, 0, len(argv))
	redactNext := false

	for _, arg := range argv {
		switch {
		case redactNext:
			out = append(out, Placeholder)
			redactNext = false

		// --flag=value and KEY=value both hide the secret after the first '='.
		case strings.Contains(arg, "="):
			name, value, _ := strings.Cut(arg, "=")
			switch {
			case isSensitiveName(name):
				out = append(out, name+"="+Placeholder)
			// An innocuous flag name can still carry a URL with a token in its
			// query string, which is the leak this would otherwise miss.
			case looksLikeURL(value):
				out = append(out, name+"="+URL(value))
			default:
				out = append(out, arg)
			}

		// A bare --password takes its value as the next argument.
		case strings.HasPrefix(arg, "-") && isSensitiveName(arg):
			out = append(out, arg)
			redactNext = true

		case looksLikeURL(arg):
			out = append(out, URL(arg))

		default:
			out = append(out, arg)
		}
	}
	return strings.Join(out, " ")
}

// Headers returns a copy with sensitive values replaced.
func Headers(h http.Header) http.Header {
	if h == nil {
		return nil
	}

	out := make(http.Header, len(h))
	for name, values := range h {
		if IsSensitiveHeader(name) {
			out[name] = []string{Placeholder}
			continue
		}
		out[name] = append([]string(nil), values...)
	}
	return out
}

// URL removes credentials and secret-looking query values from a URL.
//
// An unparseable input is returned as a placeholder rather than passed through:
// if it cannot be understood it cannot be verified safe.
func URL(raw string) string {
	if raw == "" {
		return ""
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return Placeholder
	}

	if parsed.User != nil {
		parsed.User = url.User(Placeholder)
	}

	if query := parsed.Query(); len(query) > 0 {
		changed := false
		for key := range query {
			if sensitiveQueryKeys[strings.ToLower(key)] {
				query.Set(key, Placeholder)
				changed = true
			}
		}
		if changed {
			parsed.RawQuery = query.Encode()
		}
	}

	return parsed.String()
}

// Message scrubs URLs embedded in an error string.
//
// Transport errors are formatted as `Get "http://host/path?token=x": ...`, so
// the quoted URL is extracted and redacted in place.
func Message(msg string) string {
	if msg == "" {
		return ""
	}

	var b strings.Builder
	rest := msg
	for {
		start := strings.IndexByte(rest, '"')
		if start < 0 {
			b.WriteString(rest)
			return b.String()
		}
		end := strings.IndexByte(rest[start+1:], '"')
		if end < 0 {
			b.WriteString(rest)
			return b.String()
		}
		end += start + 1

		candidate := rest[start+1 : end]
		b.WriteString(rest[:start+1])
		if looksLikeURL(candidate) {
			b.WriteString(URL(candidate))
		} else {
			b.WriteString(candidate)
		}
		b.WriteByte('"')

		rest = rest[end+1:]
	}
}

func looksLikeURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}
