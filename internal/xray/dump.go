package xray

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
)

// defaultJVMImage ships the JDK tools (jstack/jcmd/jmap) the dump steps need.
const defaultJVMImage = "eclipse-temurin:21-jdk"

func newJVMDumpCmd(configFlags *genericclioptions.ConfigFlags, streams genericiooptions.IOStreams) *cobra.Command {
	o := &Options{configFlags: configFlags, IOStreams: streams}
	var thread, histogram, heap bool
	var output string

	cmd := &cobra.Command{
		Use:   "jvm-dump <pod|deployment>",
		Short: "Capture JVM dumps (thread, GC histogram, heap) into a local dir",
		Long: `Run JVM diagnostics against the target's PID 1 from a JDK toolbox container
that shares its PID namespace and runs as the matching UID. Artifacts are
streamed out (binary-safe) into <output>/<pod>-<timestamp>/.

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
			return o.jvmDump(c.Context(), thread, histogram, heap, output)
		},
	}
	o.addCaptureFlags(cmd, defaultJVMImage)
	cmd.Flags().BoolVar(&thread, "thread", true, "Capture a thread dump (jstack)")
	cmd.Flags().BoolVar(&histogram, "histogram", true, "Capture a GC class histogram (jcmd)")
	cmd.Flags().BoolVar(&heap, "heap", true, "Capture a heap dump (jmap)")
	cmd.Flags().StringVarP(&output, "output", "o", "dumps", "Local directory to write the dump bundle into")
	return cmd
}

// jvmDump injects a JDK toolbox sharing the target's PID namespace, runs the
// enabled dump steps against PID 1, and streams a tar of the artifacts into a
// local timestamped directory.
func (o *Options) jvmDump(ctx context.Context, thread, histogram, heap bool, outDir string) error {
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

	// The container blocks on `read` until we attach and send a byte — a
	// rendezvous so its tar output can't start before our reader is connected.
	// It then runs the dumps and exits (no lingering sleep).
	script := "read _; " + buildJVMDumpScript(thread, histogram, heap, name)
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

	dest := filepath.Join(outDir, fmt.Sprintf("%s-%s", name, time.Now().Format("2006-01-02_15-04-05")))
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return fmt.Errorf("creating output dir: %w", err)
	}

	o.logf("capturing dumps into %s (heap can take a while)...", dest)

	stdoutR, stdoutW := io.Pipe()
	stdinR, stdinW := io.Pipe()

	// Attach in the background; it streams the container's stdout (the tar) into
	// stdoutW until the container exits, or we tear the stream down.
	var stderr bytes.Buffer
	attachCtx, cancelAttach := context.WithCancel(ctx)
	defer cancelAttach()
	attachDone := make(chan error, 1)
	go func() {
		err := attachToContainer(attachCtx, o.restConfig, o.clientset, o.namespace, pod.Name, ec.Name, stdinR, stdoutW, &stderr)
		_ = stdoutW.CloseWithError(err)
		attachDone <- err
	}()

	// Send the signal, but keep stdin open: an immediate EOF drops the attach
	// session before the output arrives.
	go func() { _, _ = stdinW.Write([]byte("\n")) }()

	files, exErr := extractTar(stdoutR, dest)

	// Drain trailing bytes so the attach's stdout copy finishes and returns
	// cleanly when the container exits (archive/tar stops at the end-of-archive
	// marker and leaves padding unread), then release stdin.
	_, _ = io.Copy(io.Discard, stdoutR)
	_ = stdinW.Close()

	<-attachDone // wait for the attach goroutine; its error already surfaced via exErr

	if exErr != nil {
		return fmt.Errorf("capturing dumps: %w (stderr: %s)", exErr, strings.TrimSpace(stderr.String()))
	}
	if len(files) == 0 {
		return fmt.Errorf("no dump artifacts produced (stderr: %s)", strings.TrimSpace(stderr.String()))
	}
	for _, f := range files {
		o.logf("wrote %s", f)
	}
	return nil
}

// buildJVMDumpScript writes each enabled artifact into a work dir, then tars it
// to stdout. Only tar writes to stdout; tool chatter is redirected away.
func buildJVMDumpScript(thread, histogram, heap bool, name string) string {
	var b strings.Builder
	b.WriteString(`W="$(mktemp -d)"; `)
	if thread {
		fmt.Fprintf(&b, `jstack 1 > "$W/%s.jstack" 2>/dev/null; `, name)
	}
	if histogram {
		fmt.Fprintf(&b, `jcmd 1 GC.class_histogram > "$W/%s.histogram.txt" 2>/dev/null; `, name)
	}
	if heap {
		// The JVM writes the .hprof into its own filesystem (target /tmp); read it
		// back via /proc/1/root (same UID), then stage it in the work dir.
		fmt.Fprintf(&b, `jmap -dump:live,format=b,file=/tmp/%s.hprof 1 >/dev/null 2>&1; cp /proc/1/root/tmp/%s.hprof "$W/" 2>/dev/null; `, name, name)
	}
	b.WriteString(`tar cf - -C "$W" .`)
	return b.String()
}

// extractTar writes each regular file from the tar stream into dest (flattened),
// returning the paths written.
func extractTar(r io.Reader, dest string) ([]string, error) {
	tr := tar.NewReader(r)
	var written []string
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return written, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		out := filepath.Join(dest, filepath.Base(hdr.Name)) // Base() guards against path traversal
		if err := writeFile(out, tr); err != nil {
			return written, err
		}
		written = append(written, out)
	}
	return written, nil
}

// writeFile creates path and copies r into it, checking the Close error (a
// failed flush on a write is real data loss).
func writeFile(path string, r io.Reader) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	_, err = io.Copy(f, r)
	return err
}
