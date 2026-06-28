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
// local timestamped directory.
func (o *Options) jvmDump(ctx context.Context, thread, histogram, heap, extract bool, outDir string, maxSize int64) error {
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
	// and tear down stdout before we've drained it.
	script := "read _; " + buildJVMDumpScript(thread, histogram, heap, name) + "; read _ || true"
	ec, err := buildEphemeralContainer(container, o.image, []string{"sh", "-c", script}, true, false, uid, gid)
	if err != nil {
		return err
	}
	o.logf("injecting jvm-dump toolbox %s (image %s, UID %d)...", ec.Name, o.image, *uid)
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
	o.logf("capturing dumps (heap can take a while)...")

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

	// Wait for the attach goroutine.
	<-attachDone

	if consumeErr != nil {
		return fmt.Errorf("capturing dumps: %w (stderr: %s)", consumeErr, strings.TrimSpace(stderr.String()))
	}
	if artifacts == 0 {
		return fmt.Errorf("no dump artifacts produced (stderr: %s)", strings.TrimSpace(stderr.String()))
	}
	o.logf("wrote %s (%d artifacts)", report, artifacts)
	return nil
}

// buildJVMDumpScript writes each enabled artifact into a work dir, then writes a
// gzipped tar of it to stdout. Only tar writes to stdout; tool chatter is
// redirected away.
func buildJVMDumpScript(thread, histogram, heap bool, name string) string {
	var b strings.Builder
	b.WriteString(`W="$(mktemp -d)"; `)
	// Each tool's stdout goes to a file (or /dev/null); stderr is left on fd 2 so
	// failures (e.g. a read-only /tmp) reach the client instead of being swallowed.
	// Only tar writes to stdout.
	if thread {
		_, _ = fmt.Fprintf(&b, `jstack 1 > "$W/%s.jstack"; `, name)
	}
	if histogram {
		_, _ = fmt.Fprintf(&b, `jcmd 1 GC.class_histogram > "$W/%s.histogram.txt"; `, name)
	}
	if heap {
		// The JVM writes the .hprof into its own filesystem (target /tmp); read it
		// back via /proc/1/root (same UID), then stage it in the work dir.
		// rm the heap file from the target afterward so we don't leave secrets
		// (and a multi-GB file) behind in its /tmp.
		_, _ = fmt.Fprintf(&b, `jmap -dump:live,format=b,file=/tmp/%s.hprof 1 >/dev/null; cp /proc/1/root/tmp/%s.hprof "$W/"; rm -f /proc/1/root/tmp/%s.hprof; `, name, name, name)
	}
	b.WriteString(`tar czf - -C "$W" .`)
	return b.String()
}
