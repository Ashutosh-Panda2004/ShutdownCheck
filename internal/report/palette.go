package report

import (
	"io"
	"os"
	"strings"
)

// Palette renders styled terminal text, or plain text when styling is
// unwanted.
//
// Reports get piped into files and CI logs at least as often as they are read
// on a terminal, so escape codes are opt-in by detection rather than default.
type Palette struct {
	enabled bool
}

// NewPalette decides whether to style output for the given writer.
//
// It honours NO_COLOR (https://no-color.org) and disables styling whenever the
// destination is not a character device, which covers files, pipes and CI logs.
func NewPalette(w io.Writer, force bool, disable bool) Palette {
	switch {
	case disable:
		return Palette{}
	case force:
		return Palette{enabled: true}
	case os.Getenv("NO_COLOR") != "":
		return Palette{}
	case os.Getenv("TERM") == "dumb":
		return Palette{}
	default:
		return Palette{enabled: isTerminal(w)}
	}
}

// Enabled reports whether styling is on.
func (p Palette) Enabled() bool { return p.enabled }

const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
)

func (p Palette) wrap(code, s string) string {
	if !p.enabled || s == "" {
		return s
	}
	return code + s + ansiReset
}

func (p Palette) Bold(s string) string   { return p.wrap(ansiBold, s) }
func (p Palette) Dim(s string) string    { return p.wrap(ansiDim, s) }
func (p Palette) Red(s string) string    { return p.wrap(ansiRed, s) }
func (p Palette) Green(s string) string  { return p.wrap(ansiGreen, s) }
func (p Palette) Yellow(s string) string { return p.wrap(ansiYellow, s) }
func (p Palette) Cyan(s string) string   { return p.wrap(ansiCyan, s) }

// isTerminal reports whether w is an interactive terminal.
//
// Checking the file mode avoids a dependency on x/term for what is, at this
// level of precision, a one-line question.
func isTerminal(w io.Writer) bool {
	f, ok := w.(interface{ Stat() (os.FileInfo, error) })
	if !ok {
		return false
	}

	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// pad returns s padded to width, truncating if it is already longer.
func pad(s string, width int) string {
	if len(s) >= width {
		return s[:width]
	}
	return s + strings.Repeat(" ", width-len(s))
}

// padLeft right-aligns s within width.
func padLeft(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return strings.Repeat(" ", width-len(s)) + s
}

// thousands formats an integer with separators, because "1,024 requests" reads
// faster than "1024" when scanning a report.
func thousands(n int) string {
	s := itoa(n)
	if len(s) <= 3 {
		return s
	}

	var b strings.Builder
	lead := len(s) % 3
	if lead > 0 {
		b.WriteString(s[:lead])
	}
	for i := lead; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}

	neg := n < 0
	if neg {
		n = -n
	}

	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		return "-" + string(digits)
	}
	return string(digits)
}
