package redact

import (
	"bytes"
	"strings"
	"testing"
)

func TestLine(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		wantRedac bool
	}{
		{"plain value passthrough", "HOME=/root", false},
		{"port passthrough", "PORT=8080", false},
		{"no equals passthrough", "just a log line", false},
		{"empty", "", false},
		{"key PASSWORD", "DB_PASSWORD=hunter2", true},
		{"key SECRET", "MY_SECRET=abc", true},
		{"key TOKEN", "GH_TOKEN=ghp_xxx", true},
		{"key api_key", "STRIPE_API_KEY=sk_live_123", true},
		{"key access-key", "AWS_ACCESS_KEY_ID=AKIA", true},
		{"key credential", "GOOGLE_CREDENTIALS=blob", true},
		{"key case-insensitive", "service_auth=x", true},
		{"value JWT", "ID=eyJhbGciOiJIUzI1Nited.eyJzdWIiOiIxMjmed.sig", true},
		{"value user:pass URL", "DB_URL=postgres://user:s3cr3t@host:5432/db", true},
		{"value PEM", "K=-----BEGIN RSA PRIVATE KEY-----", true},
		{"value JDBC embedded password", "DB_URL_JDBC=jdbc:postgresql://host:5432/db?user=ro&password=qR05Eei_x&sslmode=require", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, red := Line(tt.in)
			if red != tt.wantRedac {
				t.Fatalf("Line(%q) redacted=%v, want %v (out=%q)", tt.in, red, tt.wantRedac, out)
			}
			if red {
				if !strings.Contains(out, mask) {
					t.Errorf("redacted line %q missing mask", out)
				}
				if strings.Contains(out, strings.SplitN(tt.in, "=", 2)[1]) {
					t.Errorf("redacted line %q still contains the secret value", out)
				}
			} else if out != tt.in {
				t.Errorf("passthrough changed line: got %q, want %q", out, tt.in)
			}
		})
	}
}

func TestWriter(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)

	// Feed in chunks that split a line across Write calls.
	in := "HOME=/root\nDB_PASSWORD=hun"
	if _, err := w.Write([]byte(in)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("ter2\nPORT=80\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}

	got := buf.String()
	if strings.Contains(got, "hunter2") {
		t.Errorf("secret leaked across chunk boundary: %q", got)
	}
	if !strings.Contains(got, "HOME=/root") || !strings.Contains(got, "PORT=80") {
		t.Errorf("plain values not preserved: %q", got)
	}
	if w.N != 1 {
		t.Errorf("N = %d, want 1", w.N)
	}
}

func TestWriterFlushTrailingLine(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	// No trailing newline; only Flush should emit it.
	if _, err := w.Write([]byte("API_KEY=secret")); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Errorf("partial line emitted before Flush: %q", buf.String())
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); strings.Contains(got, "secret") || !strings.Contains(got, mask) {
		t.Errorf("trailing line not redacted on Flush: %q", got)
	}
}
