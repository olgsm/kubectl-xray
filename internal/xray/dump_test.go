package xray

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestCaptureScriptFencing runs the wrapper captureBundle builds around an inner
// script through a real shell: a failing step must abort before anything reaches
// stdout, and the keepalive must stay off stdout and not outlive the script.
func TestCaptureScriptFencing(t *testing.T) {
	run := func(t *testing.T, inner string) (stdout, stderr string, err error) {
		t.Helper()
		script := "set -e; read _; " + keepalive + inner + "; read _ || true"
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sh", "-c", script)
		cmd.Stdin = strings.NewReader("\n")
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		err = cmd.Run()
		if ctx.Err() != nil {
			t.Fatalf("script did not exit — keepalive leaked: %v", ctx.Err())
		}
		return out.String(), errb.String(), err
	}

	t.Run("failing step aborts with no payload", func(t *testing.T) {
		stdout, _, err := run(t, `false; printf payload`)
		if err == nil {
			t.Error("want non-zero exit when a dump step fails")
		}
		if stdout != "" {
			t.Errorf("want no stdout after a failed step; got %q", stdout)
		}
	})

	t.Run("payload passes through untouched", func(t *testing.T) {
		stdout, stderr, err := run(t, `printf payload`)
		if err != nil {
			t.Fatalf("script failed: %v (stderr %q)", err, stderr)
		}
		if stdout != "payload" {
			t.Errorf("stdout = %q, want %q — keepalive must not write to stdout", stdout, "payload")
		}
	})
}

func TestBuildJVMDumpScript(t *testing.T) {
	const name = "mypod"
	tests := []struct {
		label                             string
		thread, histogram, heap, vthreads bool
		jfr                               time.Duration
		wantContains                      []string
		wantAbsent                        []string
	}{
		{
			label: "jfr only", jfr: 60 * time.Second,
			wantContains: []string{"JFR.start", "duration=60s", "sleep 60", "/proc/1/root/tmp/mypod.jfr"},
			wantAbsent:   []string{"jstack", "jmap", "GC.class_histogram"},
		},
		{
			label: "all steps", thread: true, histogram: true, heap: true, vthreads: true,
			wantContains: []string{"jstack 1", "GC.class_histogram", "jmap -dump:live", "mypod.jstack", "mypod.hprof", "/proc/1/root/tmp/mypod.hprof", "Thread.dump_to_file -format=json", "mypod.threads.json"},
		},
		{
			label: "vthreads only", vthreads: true,
			wantContains: []string{"Thread.dump_to_file", "/proc/1/root/tmp/mypod.threads.json"},
			wantAbsent:   []string{"jstack", "jmap", "GC.class_histogram"},
		},
		{
			label: "thread only", thread: true,
			wantContains: []string{"jstack 1", "mypod.jstack"},
			wantAbsent:   []string{"GC.class_histogram", "jmap"},
		},
		{
			label: "histogram only", histogram: true,
			wantContains: []string{"GC.class_histogram"},
			wantAbsent:   []string{"jstack", "jmap"},
		},
		{
			label: "heap only", heap: true,
			wantContains: []string{"jmap -dump:live"},
			wantAbsent:   []string{"jstack", "GC.class_histogram"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			got := buildJVMDumpScript(tt.thread, tt.histogram, tt.heap, tt.vthreads, tt.jfr, name)
			if !strings.HasPrefix(got, `W="$(mktemp -d)"; `) {
				t.Errorf("script must set up a work dir first; got %q", got)
			}
			if !strings.HasSuffix(got, `tar czf - -C "$W" .`) {
				t.Errorf("script must end by gzip-tarring the work dir to stdout; got %q", got)
			}
			for _, s := range tt.wantContains {
				if !strings.Contains(got, s) {
					t.Errorf("want %q in script; got %q", s, got)
				}
			}
			for _, s := range tt.wantAbsent {
				if strings.Contains(got, s) {
					t.Errorf("did not want %q in script; got %q", s, got)
				}
			}
		})
	}
}
