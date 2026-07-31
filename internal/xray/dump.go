package xray

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"kubectl-xray/internal/archive"
)

// exitWait bounds how long we wait for the toolbox to report its exit code once
// we've closed its stdin; it only has the trailing read left to do.
const exitWait = 60 * time.Second

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

// jvmDump injects a JDK toolbox sharing the target's PID namespace, runs the
// enabled dump steps against PID 1, and streams a tar of the artifacts into a
// local timestamped directory — or, with dumpDir, leaves them in a directory
// mounted inside the target.
func (o *Options) jvmDump(ctx context.Context, thread, histogram, heap, vthreads bool, jfr time.Duration, extract bool, outDir, dumpDir string, maxSize int64) error {
	if heap {
		o.logf("warning: the heap dump holds whatever the process has in memory — credentials, tokens, customer data — and is not redacted")
	}
	script := func(name string) string {
		return buildJVMDumpScript(thread, histogram, heap, vthreads, jfr, name, dumpDir)
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
	return o.captureOn(ctx, pod, []string{"sh", "-c", "set -e; " + innerScript(name)})
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
// minutes (jmap on a large heap), and an attach stream with no traffic gets
// dropped by middleboxes between us and the apiserver — which used to kill the
// capture mid-flight. The NULs are stripped before stderr is reported.
const keepalive = `(while :; do sleep 20; printf '\000' >&2; done) & HB=$!; trap 'kill $HB 2>/dev/null || true' EXIT; `

// captureBundle injects a toolbox container running innerScript (which must
// write a gzipped tar of its artifacts to stdout), streams that tar out, and
// writes it to a local .tar.gz (or unpacks it with --extract). The script is
// fenced by reads so its output can't start before we attach or end before we
// drain it. want is the number of artifacts the enabled flags should produce;
// the capture fails unless the toolbox exits 0 and delivers exactly that many.
func (o *Options) captureBundle(ctx context.Context, label string, innerScript func(name string) string, want int, outDir string, extract bool, maxSize int64) error {
	pod, err := o.resolvePod(ctx)
	if err != nil {
		return err
	}
	container := o.resolveContainer(pod)

	uid, gid, pod, err := o.resolveUID(ctx, pod, container)
	if err != nil {
		return err
	}
	name := pod.Name

	// Leading read: wait for our go-signal so output can't start before we attach.
	// Trailing read: stay alive until we close stdin, so the container doesn't exit
	// and tear down stdout before we've drained it. set -e so a failing dump step
	// aborts with a non-zero exit instead of tarring up whatever it managed to write.
	script := "set -e; read _; " + keepalive + innerScript(name) + "; read _ || true"
	ec, err := buildEphemeralContainer(container, o.image, []string{"sh", "-c", script}, true, false, uid, gid)
	if err != nil {
		return err
	}
	o.logf("injecting %s toolbox %s (image %s, UID %d) in %s/%s...", label, ec.Name, o.image, *uid, o.namespace, name)
	pod, err = injectEphemeralContainer(ctx, o.clientset, o.namespace, pod.Name, ec)
	if err != nil {
		return err
	}
	term, err := waitForEphemeralStart(ctx, o.clientset, o.namespace, pod.Name, ec.Name)
	if err != nil {
		return err
	}
	if term != nil {
		return fmt.Errorf("toolbox %s exited before attach: %d (%s)", ec.Name, term.ExitCode, term.Reason)
	}

	base := fmt.Sprintf("%s-%s", name, time.Now().Format("2006-01-02_15-04-05"))
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("creating output dir: %w", err)
	}
	o.logf("capturing dumps...")

	stdoutR, stdoutW := io.Pipe()
	stdinR, stdinW := io.Pipe()

	// Attach in the background; it streams the container's stdout (the gzipped tar) into stdoutW
	// until the container exits, or we tear the stream down.
	var stderr bytes.Buffer
	attachCtx, cancelAttach := context.WithCancel(ctx)
	defer cancelAttach()
	attachDone := make(chan error, 1)
	go func() {
		err := attachToContainer(attachCtx, o.restConfig, o.clientset, o.namespace, pod.Name, ec.Name, stdinR, stdoutW, &stderr, false, nil)
		_ = stdoutW.CloseWithError(err)
		attachDone <- err
	}()

	// Send the signal, but keep stdin open: an immediate EOF drops the attach before the output arrives.
	go func() { _, _ = stdinW.Write([]byte("\n")) }()

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
	artifacts, consumeErr := store.Put(stdoutR)

	// We've consumed the full archive; release the trailing read so the container
	// exits, then drain to EOF so the attach's stdout copy finishes cleanly.
	_ = stdinW.Close()
	_, _ = io.Copy(io.Discard, stdoutR)

	attachErr := <-attachDone

	// The toolbox's exit code travels over the API, not the stream that carried the
	// payload, so it still answers "was this dump complete?" after the stream breaks.
	exitCtx, cancelExit := context.WithTimeout(ctx, exitWait)
	defer cancelExit()
	term, exitErr := waitForEphemeralExit(exitCtx, o.clientset, o.namespace, pod.Name, ec.Name)

	msg := strings.TrimSpace(strings.ReplaceAll(stderr.String(), "\x00", ""))
	switch {
	case exitErr != nil:
		return fmt.Errorf("could not confirm %s completed (%v) — treat %s as incomplete (stderr: %s)", ec.Name, exitErr, report, msg)
	case term.ExitCode != 0:
		return fmt.Errorf("%s exited %d — dump is incomplete, discard %s (stderr: %s)", ec.Name, term.ExitCode, report, msg)
	case consumeErr != nil:
		return fmt.Errorf("capturing dumps: %w (stderr: %s)", consumeErr, msg)
	case attachErr != nil:
		return fmt.Errorf("dump stream broke before it was fully read: %w (stderr: %s)", attachErr, msg)
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

// buildJVMDumpScript writes each enabled artifact into a work dir, then writes a
// gzipped tar of it to stdout. Only tar writes to stdout; tool chatter is
// redirected away.
func buildJVMDumpScript(thread, histogram, heap, vthreads bool, jfr time.Duration, name, dumpDir string) string {
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
		if dumpDir == "" {
			_, _ = fmt.Fprintf(&b, `cp /proc/1/root/tmp/%s.jfr "$W/"; rm -f /proc/1/root/tmp/%s.jfr; `, name, name)
		}
	}
	b.WriteString(tailFor(dumpDir))
	return b.String()
}
