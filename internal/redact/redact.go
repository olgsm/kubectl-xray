package redact

import (
	"bytes"
	"io"
	"regexp"
	"strings"
)

const mask = "***REDACTED***"

// keyRe matches env var names that conventionally hold secrets.
var keyRe = regexp.MustCompile(`(?i)(secret|token|passwd|password|pwd|api[_-]?key|access[_-]?key|credential|auth|private[_-]?key|cert|salt|dsn|connection)`)

// valueRes match secret-shaped values regardless of key name.
var valueRes = []*regexp.Regexp{
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.`),       // JWT
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),              // PEM private key
	regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.\-]*://[^/\s:@]+:[^/\s:@]+@`), // user:pass@host URL
	regexp.MustCompile(`(?i)(password|passwd|pwd|secret|token)=[^\s&;]`),  // embedded credential, e.g. JDBC ?password=
}

// Line redacts the value of a KEY=VALUE line when the key or value looks
// secret. Lines without '=' pass through unchanged. Returns whether it redacted.
func Line(line string) (string, bool) {
	k, v, ok := strings.Cut(line, "=")
	if !ok {
		return line, false
	}
	if keyRe.MatchString(k) || matchesValue(v) {
		return k + "=" + mask, true
	}
	return line, false
}

func matchesValue(v string) bool {
	for _, re := range valueRes {
		if re.MatchString(v) {
			return true
		}
	}
	return false
}

// Writer redacts KEY=VALUE lines as they stream through to w. Call Flush after
// the last Write to emit any trailing line without a newline.
type Writer struct {
	w   io.Writer
	buf bytes.Buffer
	N   int // number of values redacted
}

func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

func (rw *Writer) Write(p []byte) (int, error) {
	rw.buf.Write(p)
	for {
		line, err := rw.buf.ReadString('\n')
		if err != nil { // no complete line yet; keep the remainder buffered
			rw.buf.Reset()
			rw.buf.WriteString(line)
			break
		}
		if werr := rw.emit(strings.TrimSuffix(line, "\n"), "\n"); werr != nil {
			return 0, werr
		}
	}
	return len(p), nil
}

// Flush redacts and writes any buffered partial line (no trailing newline).
func (rw *Writer) Flush() error {
	if rw.buf.Len() == 0 {
		return nil
	}
	line := rw.buf.String()
	rw.buf.Reset()
	return rw.emit(line, "")
}

func (rw *Writer) emit(line, suffix string) error {
	out, redacted := Line(line)
	if redacted {
		rw.N++
	}
	_, err := io.WriteString(rw.w, out+suffix)
	return err
}
