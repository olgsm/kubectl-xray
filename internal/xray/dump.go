package xray

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	utilexec "k8s.io/client-go/util/exec"

	"github.com/olgsm/kubectl-xray/internal/archive"
	"github.com/olgsm/kubectl-xray/internal/redact"
)

// Store persists a captured dump stream. Named for persistence so a future
// backend (e.g. an S3 uploader) can satisfy it alongside the local strategies.
type Store interface {
	// Put consumes the gzipped-tar stream r and returns the number of artifacts
	// persisted (0 means the dump produced nothing).
	Put(r io.Reader) (artifacts int, err error)
}

// archiveStore writes one portable .tar.gz at path. maxSize caps the bundle in
// bytes (0 = unlimited).
type archiveStore struct {
	path    string
	maxSize int64
}

func (s archiveStore) Put(r io.Reader) (int, error) { return archive.Write(r, s.path, s.maxSize) }

// dirStore unpacks the dump into dest (the --extract strategy). maxSize caps the
// total extracted bytes (0 = unlimited).
type dirStore struct {
	dest    string
	maxSize int64
}

func (s dirStore) Put(r io.Reader) (int, error) {
	if err := os.MkdirAll(s.dest, 0o755); err != nil {
		return 0, err
	}
	files, err := archive.Extract(r, s.dest, s.maxSize)
	return len(files), err
}

// redactStore scrubs heap dumps in the stream (see redact.Bundle) before inner
// persists it, so no unredacted hprof is ever written locally.
type redactStore struct{ inner Store }

func (s redactStore) Put(r io.Reader) (int, error) {
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := redact.Bundle(r, pw)
		_ = pw.CloseWithError(err)
		done <- err
	}()
	n, err := s.inner.Put(pr)
	_ = pr.Close() // unblock Bundle if inner stopped early
	if rerr := <-done; rerr != nil && !errors.Is(rerr, io.ErrClosedPipe) {
		return n, fmt.Errorf("redacting dump: %w (--no-redact keeps it raw)", rerr)
	}
	return n, err
}

// jvmDump injects a JDK toolbox sharing the target's PID namespace, runs the
// enabled dump steps against PID 1, and streams a tar of the artifacts into a
// local timestamped directory — or, with dumpDir, leaves them in a directory
// mounted inside the target.
func (o *Options) jvmDump(ctx context.Context, thread, histogram, heap, vthreads bool, jfr time.Duration, extract bool, outDir, dumpDir string, maxSize int64) error {
	if dumpDir != "" {
		o.redact = false
	}
	if (heap || jfr > 0) && !o.redact {
		o.logf("warning: heap dumps and JFR recordings are not redacted with --no-redact or --dump-dir — they can carry credentials, tokens, customer data")
	}
	script := func(name string) string {
		return buildJVMDumpScript(thread, histogram, heap, vthreads, jfr, name, dumpDir, o.redact)
	}
	if dumpDir != "" {
		return o.captureInPod(ctx, "jvm-dump", script, dumpDir)
	}
	return o.captureBundle(ctx, "jvm-dump", script, countTrue(thread, histogram, heap, vthreads, jfr > 0), outDir, extract, maxSize)
}

// captureInPod runs the dump steps but leaves the artifacts in dumpDir inside
// the target, for whatever watches that directory to pick them up.
// Nothing is streamed back except the script's progress output.
func (o *Options) captureInPod(ctx context.Context, label string, innerScript func(name string) string, dumpDir string) error {
	pod, err := o.resolvePod(ctx)
	if err != nil {
		return err
	}
	o.logf("%s: writing artifacts into %s inside %s/%s; nothing is streamed out", label, dumpDir, o.namespace, pod.Name)
	name := fmt.Sprintf("%s-%s", pod.Name, time.Now().Format("2006-01-02_15-04-05"))
	return o.captureOn(ctx, pod, label, []string{"sh", "-c", "set -e; " + innerScript(name)})
}

func countTrue(flags ...bool) int {
	n := 0
	for _, f := range flags {
		if f {
			n++
		}
	}
	return n
}

// goDump injects a toolbox sharing the target's network namespace and scrapes
// the app's pprof endpoints over localhost into a local bundle. PoC.
func (o *Options) goDump(ctx context.Context, port string, goroutine, heap, profile, extract bool, outDir, dumpDir string, maxSize int64) error {
	script := func(name string) string {
		return buildGoDumpScript(port, goroutine, heap, profile, name, dumpDir)
	}
	if dumpDir != "" {
		return o.captureInPod(ctx, "go-dump", script, dumpDir)
	}
	return o.captureBundle(ctx, "go-dump", script, countTrue(goroutine, heap, profile), outDir, extract, maxSize)
}

// keepalive emits a NUL on stderr every 20s. A dump tool can work silently for
// minutes (jmap on a large heap), and an exec stream with no traffic gets
// dropped by middleboxes between us and the apiserver — which used to kill the
// capture mid-flight. The NULs are stripped before stderr is reported.
// It must leave no process behind: an orphan is reparented to the target's PID 1,
// which typically never reaps it and keeps it as a zombie. Hence one-second
// foreground sleeps, which the TERM trap waits out, rather than a background one.
const keepalive = `(trap exit TERM; while :; do i=0; while [ $i -lt 20 ]; do sleep 1; i=$((i+1)); done; printf '\000' >&2; done) & HB=$!; trap 'kill $HB 2>/dev/null; wait $HB 2>/dev/null || true' EXIT; `

// captureBundle runs innerScript (which must write a gzipped tar of its
// artifacts to stdout) in a toolbox beside the target, and writes the tar to a
// local .tar.gz (or unpacks it with --extract). want is the number of artifacts
// the enabled flags should produce; the capture fails unless the script exits 0
// and delivers exactly that many.
func (o *Options) captureBundle(ctx context.Context, label string, innerScript func(name string) string, want int, outDir string, extract bool, maxSize int64) error {
	pod, err := o.resolvePod(ctx)
	if err != nil {
		return err
	}
	name := pod.Name
	pod, ecName, err := o.startToolbox(ctx, pod, label)
	if err != nil {
		return err
	}

	base := fmt.Sprintf("%s-%s", name, time.Now().Format("2006-01-02_15-04-05"))
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("creating output dir: %w", err)
	}
	o.logf("capturing dumps...")

	// set -e so a failing dump step aborts with a non-zero exit instead of
	// tarring up whatever it managed to write.
	command := []string{"sh", "-c", "set -e; " + keepalive + innerScript(name)}
	stdoutR, stdoutW := io.Pipe()
	var stderr bytes.Buffer
	execCtx, cancelExec := context.WithCancel(ctx)
	defer cancelExec()
	execDone := make(chan error, 1)
	go func() {
		err := execInContainer(execCtx, o.restConfig, o.clientset, o.namespace, pod.Name, ecName, inToolbox(command...), nil, stdoutW, &stderr, false, nil)
		_ = stdoutW.CloseWithError(err)
		execDone <- err
	}()

	// Default: write one portable .tar.gz. --extract: unpack into a directory.
	var store Store
	var report string
	if extract {
		report = filepath.Join(outDir, base)
		store = dirStore{dest: report, maxSize: maxSize}
	} else {
		report = filepath.Join(outDir, base+".tar.gz")
		store = archiveStore{path: report, maxSize: maxSize}
	}
	if o.redact {
		store = redactStore{store}
	}
	artifacts, consumeErr := store.Put(stdoutR)
	if consumeErr != nil {
		cancelExec()
	}
	_, _ = io.Copy(io.Discard, stdoutR)
	execErr := <-execDone

	msg := strings.TrimSpace(strings.ReplaceAll(stderr.String(), "\x00", ""))
	var exitErr utilexec.ExitError
	switch {
	case errors.As(execErr, &exitErr):
		return fmt.Errorf("%s exited %d — dump is incomplete, discard %s (stderr: %s)", ecName, exitErr.ExitStatus(), report, msg)
	case consumeErr != nil:
		return fmt.Errorf("capturing dumps: %w (stderr: %s)", consumeErr, msg)
	case execErr != nil:
		return fmt.Errorf("dump stream broke before it was fully read — treat %s as incomplete: %w (stderr: %s)", report, execErr, msg)
	case artifacts != want:
		return fmt.Errorf("expected %d dump artifacts, got %d — discard %s (stderr: %s)", want, artifacts, report, msg)
	}
	o.logf("wrote %s (%d artifacts, toolbox exited 0)", report, artifacts)
	return nil
}

// workDirFor opens a dump script by pointing $W at the destination: a private
// staging dir when the bundle is streamed back, or — with dumpDir — the target's
// own directory, reached through its mount namespace via /proc/1/root.
func workDirFor(dumpDir string) string {
	if dumpDir == "" {
		return `W="$(mktemp -d)"; `
	}
	return fmt.Sprintf(`W=/proc/1/root%s; [ -d "$W" ] || { echo "%s is not a directory in the target" >&2; exit 1; }; `, dumpDir, dumpDir)
}

// tailFor closes a dump script: tar the staging dir to stdout, or just list what
// was left in the target's directory.
func tailFor(dumpDir string) string {
	if dumpDir == "" {
		return `tar czf - -C "$W" .`
	}
	return `ls -l "$W"`
}

// buildGoDumpScript scrapes the app's pprof endpoints on localhost:port into a
// work dir, then writes a gzipped tar of it to stdout. Requires the app to
// serve net/http/pprof on that port. wget stderr is left on fd 2 so a refused
// connection surfaces instead of being swallowed.
func buildGoDumpScript(port string, goroutine, heap, profile bool, name, dumpDir string) string {
	var b strings.Builder
	b.WriteString(workDirFor(dumpDir))
	base := "http://localhost:" + port + "/debug/pprof/"
	if goroutine {
		_, _ = fmt.Fprintf(&b, `wget -O "$W/%s.goroutine.txt" "%sgoroutine?debug=2"; `, name, base)
	}
	if heap {
		_, _ = fmt.Fprintf(&b, `wget -O "$W/%s.heap.pprof" "%sheap"; `, name, base)
	}
	if profile {
		_, _ = fmt.Fprintf(&b, `wget -O "$W/%s.cpu.pprof" "%sprofile?seconds=10"; `, name, base)
	}
	b.WriteString(tailFor(dumpDir))
	return b.String()
}

// jfrScrubFilters keep only JDK events (app-defined ones can carry anything) and
// drop those recording env vars, system/security properties, the command line,
// other processes' command lines and exception messages.
const jfrScrubFilters = `--include-events 'jdk.*' --exclude-events jdk.InitialEnvironmentVariable,jdk.InitialSystemProperty,jdk.InitialSecurityProperty,jdk.SecurityPropertyModification,jdk.JVMInformation,jdk.SystemProcess,jdk.ProcessStart,jdk.JavaExceptionThrow,jdk.JavaErrorThrow`

// buildJVMDumpScript writes each enabled artifact into a work dir, then writes a
// gzipped tar of it to stdout. Only tar writes to stdout; tool chatter is
// redirected away.
func buildJVMDumpScript(thread, histogram, heap, vthreads bool, jfr time.Duration, name, dumpDir string, scrub bool) string {
	var b strings.Builder
	b.WriteString(workDirFor(dumpDir))
	// jvmPath is how the target JVM addresses the destination; jvmSeen is how the
	// toolbox sees the files the JVM writes there.
	jvmPath, jvmSeen := "/tmp", "/proc/1/root/tmp"
	if dumpDir != "" {
		jvmPath, jvmSeen = dumpDir, `"$W"`
	}
	// Each tool's stdout goes to a file (or /dev/null); stderr is left on fd 2 so
	// failures (e.g. a read-only /tmp) reach the client instead of being swallowed.
	// Only tar writes to stdout.
	if thread {
		_, _ = fmt.Fprintf(&b, `jstack 1 > "$W/%s.jstack"; `, name)
	}
	if histogram {
		_, _ = fmt.Fprintf(&b, `jcmd 1 GC.class_histogram > "$W/%s.histogram.txt"; `, name)
	}
	if vthreads {
		// Virtual threads don't appear in a jstack dump; Thread.dump_to_file (JDK 21+)
		// is the only view of them.
		_, _ = fmt.Fprintf(&b, `jcmd 1 Thread.dump_to_file -format=json %s/%s.threads.json >/dev/null; `, jvmPath, name)
		if dumpDir == "" {
			_, _ = fmt.Fprintf(&b, `cp /proc/1/root/tmp/%s.threads.json "$W/"; rm -f /proc/1/root/tmp/%s.threads.json; `, name, name)
		}
	}
	if heap {
		// The JVM writes the .hprof into its own filesystem. When staging, read it
		// back via /proc/1/root (same UID) and rm it afterwards so we don't leave
		// secrets (and a multi-GB file) behind in the target's /tmp.
		_, _ = fmt.Fprintf(&b, `jmap -dump:live,format=b,file=%s/%s.hprof 1 >/dev/null; `, jvmPath, name)
		if dumpDir == "" {
			_, _ = fmt.Fprintf(&b, `cp /proc/1/root/tmp/%s.hprof "$W/"; rm -f /proc/1/root/tmp/%s.hprof; `, name, name)
		}
	}
	if jfr > 0 {
		// The recording stops itself after duration= and only then writes the file.
		// Poll for it rather than guessing a margin.
		secs := int(jfr.Seconds())
		_, _ = fmt.Fprintf(&b, `jcmd 1 JFR.start name=xray settings=profile duration=%ds filename=%s/%s.jfr >/dev/null; sleep %d; `, secs, jvmPath, name, secs)
		_, _ = fmt.Fprintf(&b, `i=0; while [ ! -f %s/%s.jfr ] && [ $i -lt 30 ]; do sleep 1; i=$((i+1)); done; `, jvmSeen, name)
		switch {
		case dumpDir != "":
		case scrub:
			_, _ = fmt.Fprintf(&b, `jfr scrub %s /proc/1/root/tmp/%s.jfr "$W/%s.jfr" >/dev/null; rm -f /proc/1/root/tmp/%s.jfr; `, jfrScrubFilters, name, name, name)
		default:
			_, _ = fmt.Fprintf(&b, `cp /proc/1/root/tmp/%s.jfr "$W/"; rm -f /proc/1/root/tmp/%s.jfr; `, name, name)
		}
	}
	b.WriteString(tailFor(dumpDir))
	return b.String()
}
