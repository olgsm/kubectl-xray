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

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"

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

// defaultJVMImage ships the JDK tools (jstack/jcmd/jmap) the dump steps need.
const defaultJVMImage = "eclipse-temurin:21-jdk"

func newJVMDumpCmd(configFlags *genericclioptions.ConfigFlags, streams genericiooptions.IOStreams) *cobra.Command {
	o := &Options{configFlags: configFlags, IOStreams: streams}
	var thread, histogram, heap, extract bool
	var output, maxSize string

	cmd := &cobra.Command{
		Use:   "jvm-dump <pod|deployment>",
		Short: "Capture JVM dumps (thread, GC histogram, heap) into a local bundle",
		Long: `Run JVM diagnostics against the target's PID 1 from a JDK toolbox container
that shares its PID namespace and runs as the matching UID. Artifacts are
streamed out (binary-safe) as a single <output>/<pod>-<timestamp>.tar.gz —
easy to share or attach to an incident. Use --extract to unpack into a
directory instead, and --max-size to fail rather than fill local disk on a
multi-GB heap.

JFR profiling is not included yet.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if !thread && !histogram && !heap {
				return fmt.Errorf("nothing to dump: enable at least one of --thread, --histogram, --heap")
			}
			if err := o.Complete(c, args); err != nil {
				return err
			}
			if err := o.Validate(); err != nil {
				return err
			}
			maxBytes, err := parseMaxSize(maxSize)
			if err != nil {
				return err
			}
			return o.jvmDump(c.Context(), thread, histogram, heap, extract, output, maxBytes)
		},
	}
	o.addCaptureFlags(cmd, defaultJVMImage)
	cmd.Flags().BoolVar(&thread, "thread", true, "Capture a thread dump (jstack)")
	cmd.Flags().BoolVar(&histogram, "histogram", true, "Capture a GC class histogram (jcmd)")
	cmd.Flags().BoolVar(&heap, "heap", true, "Capture a heap dump (jmap)")
	cmd.Flags().BoolVar(&extract, "extract", false, "Unpack dump bundle into output directory instead of writing a single .tar.gz")
	cmd.Flags().StringVarP(&output, "output", "o", "dumps", "Local directory to write the dump bundle into")
	cmd.Flags().StringVar(&maxSize, "max-size", "", "Fail if the dump exceeds this size (e.g. 2Gi); empty means unlimited")
	return cmd
}

// parseMaxSize converts a human-readable size (e.g. "2Gi", "500M") into bytes.
// An empty string means unlimited (0).
func parseMaxSize(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0, fmt.Errorf("invalid --max-size %q: %w", s, err)
	}
	if q.Sign() < 0 {
		return 0, fmt.Errorf("invalid --max-size %q: must not be negative", s)
	}
	return q.Value(), nil
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
	if thread {
		_, _ = fmt.Fprintf(&b, `jstack 1 > "$W/%s.jstack" 2>/dev/null; `, name)
	}
	if histogram {
		_, _ = fmt.Fprintf(&b, `jcmd 1 GC.class_histogram > "$W/%s.histogram.txt" 2>/dev/null; `, name)
	}
	if heap {
		// The JVM writes the .hprof into its own filesystem (target /tmp); read it
		// back via /proc/1/root (same UID), then stage it in the work dir.
		_, _ = fmt.Fprintf(&b, `jmap -dump:live,format=b,file=/tmp/%s.hprof 1 >/dev/null 2>&1; cp /proc/1/root/tmp/%s.hprof "$W/" 2>/dev/null; `, name, name)
	}
	b.WriteString(`tar czf - -C "$W" .`)
	return b.String()
}
