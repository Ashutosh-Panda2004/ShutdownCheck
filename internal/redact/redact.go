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
	"regexp"
	"strings"
	"unicode"
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

var (
	embeddedURL        = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s"'<>]+`)
	ansiEscape         = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07]*(?:\x07|\x1b\\))`)
	sensitiveLogHeader = regexp.MustCompile(`(?i)\b(authorization|proxy-authorization|cookie|set-cookie|x-api-key|x-auth-token|x-csrf-token)([ \t]*:[ \t]*)([^\r\n]*)`)
	sensitiveLogValue  = regexp.MustCompile(`(?i)\b([a-z0-9_-]*(?:access[_-]?token|api[_-]?key|apikey|auth|credential|passwd|password|private[_-]?key|pwd|secret|token)[a-z0-9_-]*)(["']?[ \t]*[:=][ \t]*)(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s,;]+)`)
)

// IsSensitiveHeader reports whether a header must be redacted.
func IsSensitiveHeader(name string) bool {
	return sensitiveHeaders[strings.ToLower(strings.TrimSpace(name))]
}

// sensitiveWords mark a flag or variable whose value must not be recorded.
// Matched as substrings, so --db-password and DATABASE_TOKEN are both caught.
var sensitiveWords = []string{
	"apikey", "api-key", "api_key",
	"auth", "credential", "passwd", "password",
	"private", "pwd", "secret", "signature", "token",
}

func isSensitiveName(name string) bool {
	lower := strings.ToLower(strings.TrimLeft(Text(name), "-"))
	if lower == "key" || strings.HasSuffix(lower, "-key") || strings.HasSuffix(lower, "_key") {
		return true
	}
	compact := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return -1
	}, lower)
	for _, word := range sensitiveWords {
		normalizedWord := strings.NewReplacer("-", "", "_", "").Replace(word)
		if strings.Contains(compact, normalizedWord) {
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
		arg = Text(arg)
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
				out = append(out, name+"="+Message(value))
			}

		// --token:value and /password:value are common in cross-platform CLIs.
		case strings.Contains(arg, ":") && (strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "/")):
			name, _, _ := strings.Cut(arg, ":")
			if isSensitiveName(strings.TrimLeft(name, "/")) {
				out = append(out, name+":"+Placeholder)
			} else {
				out = append(out, arg)
			}

		// A bare --password takes its value as the next argument.
		case strings.HasPrefix(arg, "-") && isSensitiveName(arg):
			out = append(out, arg)
			redactNext = true

		case looksLikeURL(arg):
			out = append(out, URL(arg))

		default:
			out = append(out, Message(arg))
		}
	}
	return Text(strings.Join(out, " "))
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
		out[name] = make([]string, len(values))
		for i, value := range values {
			out[name][i] = Text(value)
		}
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
	raw = Text(raw)

	parsed, err := url.Parse(raw)
	if err != nil {
		return Placeholder
	}

	if parsed.User != nil {
		parsed.User = url.User(Placeholder)
	}
	// Fragments are never sent in an HTTP request and can only leak local data
	// into reports, so there is no diagnostic value in retaining them.
	parsed.Fragment = ""
	parsed.RawFragment = ""

	if parsed.RawQuery != "" {
		query, err := url.ParseQuery(parsed.RawQuery)
		if err != nil {
			parsed.RawQuery = "redacted=" + url.QueryEscape(Placeholder)
			return Text(parsed.String())
		}
		changed := false
		for key := range query {
			if sensitiveQueryKeys[strings.ToLower(key)] || isSensitiveName(key) {
				query.Set(key, Placeholder)
				changed = true
			}
		}
		if changed {
			parsed.RawQuery = query.Encode()
		}
	}

	return Text(parsed.String())
}

// Message scrubs URLs, headers and key/value credentials in a log line.
func Message(msg string) string {
	if msg == "" {
		return ""
	}
	msg = Text(msg)
	msg = embeddedURL.ReplaceAllStringFunc(msg, URL)

	var b strings.Builder
	rest := msg
	for {
		start := strings.IndexByte(rest, '"')
		if start < 0 {
			b.WriteString(rest)
			break
		}
		end := strings.IndexByte(rest[start+1:], '"')
		if end < 0 {
			b.WriteString(rest)
			break
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

	redacted := sensitiveLogHeader.ReplaceAllString(b.String(), "${1}${2}"+Placeholder)
	return Text(sensitiveLogValue.ReplaceAllString(redacted, "${1}${2}"+Placeholder))
}

// Text removes terminal controls and bidirectional formatting characters from
// untrusted text while preserving its printable diagnostic content.
func Text(value string) string {
	value = ansiEscape.ReplaceAllString(value, "")
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || (r >= '\u202a' && r <= '\u202e') || (r >= '\u2066' && r <= '\u2069') {
			return ' '
		}
		return r
	}, value)
}

func looksLikeURL(s string) bool {
	parsed, err := url.Parse(s)
	return err == nil && parsed.Scheme != "" && strings.HasPrefix(s, parsed.Scheme+"://")
}
