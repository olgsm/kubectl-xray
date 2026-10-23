package xray

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestToolboxLifecycle runs the toolbox's idle loop and an exec'd capture
// through a real shell: a failing dump step aborts before any payload, the
// payload and exit code pass through untouched, the keepalive doesn't outlive
// the capture, and the idle toolbox exits once the capture is done, or soon
// after start if no capture ever runs.
func TestToolboxLifecycle(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := func(args []string) *exec.Cmd {
		for i := range args {
			args[i] = strings.NewReplacer(doneFile, filepath.Join(dir, "done"), startedFile, filepath.Join(dir, "started")).Replace(args[i])
		}
		return exec.CommandContext(ctx, args[0], args[1:]...)
	}
	capture := func(t *testing.T, inner string) (stdout string, code int) {
		t.Helper()
		cmd := command(inToolbox("sh", "-c", "set -e; "+keepalive+inner))
		var out bytes.Buffer
		cmd.Stdout = &out
		err := cmd.Run()
		if ctx.Err() != nil {
			t.Fatalf("capture did not exit — keepalive leaked: %v", ctx.Err())
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return out.String(), exitErr.ExitCode()
		}
		if err != nil {
			t.Fatal(err)
		}
		return out.String(), 0
	}

	unused := command(idleCommand(time.Second))
	if err := unused.Run(); err == nil || ctx.Err() != nil {
		t.Errorf("idle toolbox with no capture: want a non-zero exit after the timeout, got %v (%v)", err, ctx.Err())
	}

	idle := command(idleCommand(time.Second))
	if err := idle.Start(); err != nil {
		t.Fatal(err)
	}
	// Outlasts the timeout: a started capture must keep the toolbox alive.
	if out, code := capture(t, `sleep 2; printf payload`); code != 0 || out != "payload" {
		t.Errorf("exit %d, stdout %q; want 0 and the payload untouched", code, out)
	}
	if err := idle.Wait(); err != nil || ctx.Err() != nil {
		t.Errorf("idle toolbox did not exit cleanly after the capture: %v (%v)", err, ctx.Err())
	}
	if out, code := capture(t, `false; printf payload`); code != 1 || out != "" {
		t.Errorf("failing step: exit %d, stdout %q; want 1 and no payload", code, out)
	}
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
			got := buildJVMDumpScript(tt.thread, tt.histogram, tt.heap, tt.vthreads, tt.jfr, name, "", false)
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

// With --dump-dir the artifacts stay in the target: the JVM writes its own files
// straight to that path (no staging copy, no rm), the toolbox reaches the same
// directory through /proc/1/root, and nothing is tarred to stdout.
func TestBuildJVMDumpScriptDumpDir(t *testing.T) {
	got := buildJVMDumpScript(true, false, true, true, 30*time.Second, "mypod", "/dumps", false)

	for _, want := range []string{
		`W=/proc/1/root/dumps`,
		`jmap -dump:live,format=b,file=/dumps/mypod.hprof 1`,
		`Thread.dump_to_file -format=json /dumps/mypod.threads.json`,
		`filename=/dumps/mypod.jfr`,
		`ls -l "$W"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("want %q in script; got %q", want, got)
		}
	}
	for _, unwanted := range []string{"mktemp", "tar czf", "cp /proc/1/root/tmp", "rm -f /proc/1/root/tmp"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("did not want %q with --dump-dir; got %q", unwanted, got)
		}
	}
}

func TestBuildGoDumpScriptDumpDir(t *testing.T) {
	got := buildGoDumpScript("6060", true, true, false, "mypod", "/dumps")
	if !strings.Contains(got, `W=/proc/1/root/dumps`) || !strings.HasSuffix(got, `ls -l "$W"`) {
		t.Errorf("want the target dir as work dir and no tar; got %q", got)
	}
	if strings.Contains(got, "tar czf") {
		t.Errorf("did not want a tar with --dump-dir; got %q", got)
	}
}

func TestBuildJVMDumpScriptScrubsJFR(t *testing.T) {
	got := buildJVMDumpScript(false, false, false, false, 30*time.Second, "mypod", "", true)
	if !strings.Contains(got, `jfr scrub --include-events 'jdk.*' --exclude-events jdk.InitialEnvironmentVariable,`) ||
		!strings.Contains(got, `/proc/1/root/tmp/mypod.jfr "$W/mypod.jfr" >/dev/null`) {
		t.Errorf("want the JFR scrubbed into the work dir; got %q", got)
	}
	if strings.Contains(got, "cp /proc/1/root/tmp/mypod.jfr") {
		t.Errorf("did not want the raw JFR copied; got %q", got)
	}
}
