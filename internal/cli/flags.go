package cli

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/probe"
	"github.com/Ashutosh-Panda2004/ShutdownCheck/internal/redact"
)

// repeatedString collects a flag given more than once.
type repeatedString []string

func (r *repeatedString) String() string { return strings.Join(*r, ",") }

func (r *repeatedString) Set(value string) error {
	if value == "" {
		return fmt.Errorf("value must not be empty")
	}
	*r = append(*r, value)
	return nil
}

// idList collects signature identifiers, accepting either repetition or a
// comma-separated list so that `--ignore SC009,SC014` behaves as people expect.
type idList []string

func (l *idList) String() string { return strings.Join(*l, ",") }

func (l *idList) Set(value string) error {
	for _, part := range strings.Split(value, ",") {
		part = strings.ToUpper(strings.TrimSpace(part))
		if part == "" {
			continue
		}
		*l = append(*l, part)
	}
	return nil
}

// headerList collects repeated `--header Name: value` flags.
type headerList map[string]string

func (h *headerList) String() string {
	if h == nil || *h == nil {
		return ""
	}

	parts := make([]string, 0, len(*h))
	for name := range *h {
		parts = append(parts, name+": "+redact.Placeholder)
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

func (h *headerList) Set(value string) error {
	name, headerValue, ok := strings.Cut(value, ":")
	if !ok {
		return fmt.Errorf("expected `Name: value`, got %q", value)
	}

	name = strings.TrimSpace(name)
	headerValue = strings.TrimSpace(headerValue)
	if !probe.ValidHeaderName(name) {
		return fmt.Errorf("header name is not a valid HTTP token")
	}
	if !probe.ValidHeaderValue(headerValue) {
		return fmt.Errorf("header value must not contain control characters")
	}

	if *h == nil {
		*h = headerList{}
	}
	(*h)[http.CanonicalHeaderKey(name)] = headerValue
	return nil
}
