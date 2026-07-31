package xray

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
)

// defaultToolboxImage is the debug image for env — small, has sh/tr.
const defaultToolboxImage = "busybox:1.36"

// defaultJVMImage ships the JDK tools (jstack/jcmd/jmap) the dump steps need.
const defaultJVMImage = "eclipse-temurin:21-jdk"

func (o *Options) addCaptureFlags(cmd *cobra.Command, defaultImage string) {
	cmd.Flags().StringVarP(&o.container, "container", "c", "", "Name of the container")
	cmd.Flags().StringVar(&o.image, "image", defaultImage, "Toolbox image for the debug container")
	cmd.Flags().Int64Var(&o.asUser, "run-as-user", 0, "Run the debug container as this UID (overrides the UID derived from the target)")
}

// resolveSteps makes the dump-step flags additive: naming any of them selects
// exactly that set, and naming none enables byDefault. Without this the flags
// would have to default to true, leaving --heap a no-op and --heap=false the
// only way to say anything.
func resolveSteps(fs *pflag.FlagSet, names []string, byDefault ...*bool) {
	for _, n := range names {
		if fs.Changed(n) {
			return
		}
	}
	for _, p := range byDefault {
		*p = true
	}
}

// runCapture runs the Complete/Validate/capture lifecycle for a command.
func (o *Options) runCapture(c *cobra.Command, args, command []string) error {
	if err := o.Complete(c, args); err != nil {
		return err
	}
	if err := o.Validate(); err != nil {
		return err
	}
	return o.capture(c.Context(), command)
}

func NewCmdXRay(streams genericiooptions.IOStreams) *cobra.Command {
	configFlags := genericclioptions.NewConfigFlags(true)

	root := &cobra.Command{
		Use:           "kubectl-xray",
		Short:         "Inspect pod environments and capture execution dumps",
		SilenceUsage:  true, // don't dump usage on a runtime error
		SilenceErrors: true, // we print the error ourselves in main
	}
	root.CompletionOptions.DisableDefaultCmd = true
	configFlags.AddFlags(root.PersistentFlags())

	root.AddCommand(newEnvCmd(configFlags, streams))
	root.AddCommand(newInfoCmd(configFlags, streams))
	root.AddCommand(newJVMDumpCmd(configFlags, streams))
	root.AddCommand(newGoDumpCmd(configFlags, streams))
	root.AddCommand(newDebugCmd(configFlags, streams))

	return root
}

func newGoDumpCmd(configFlags *genericclioptions.ConfigFlags, streams genericiooptions.IOStreams) *cobra.Command {
	o := &Options{configFlags: configFlags, IOStreams: streams}
	var goroutine, heap, profile, extract bool
	var output, port, maxSize string

	cmd := &cobra.Command{
		Use:   "go-dump <pod|deployment>",
		Short: "Capture Go pprof profiles (goroutine, heap, CPU) into a local bundle",
		Long: `Scrape the app's net/http/pprof endpoints over localhost from a toolbox
container sharing the pod's network namespace, and stream them into a single
<output>/<pod>-<timestamp>.tar.gz. Requires the app to serve pprof on --port.

With no step flags the goroutine dump and heap profile are captured; naming
any of --goroutine, --heap or --profile captures only those.

PoC: no delve/core support (those need CAP_SYS_PTRACE).`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			resolveSteps(c.Flags(), []string{"goroutine", "heap", "profile"}, &goroutine, &heap)
			if !goroutine && !heap && !profile {
				return fmt.Errorf("nothing to dump: enable at least one of --goroutine, --heap, --profile")
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
			return o.goDump(c.Context(), port, goroutine, heap, profile, extract, output, maxBytes)
		},
	}
	o.addCaptureFlags(cmd, defaultToolboxImage)
	cmd.Flags().StringVar(&port, "port", "6060", "Port where the app serves net/http/pprof")
	cmd.Flags().BoolVar(&goroutine, "goroutine", false, "Capture a goroutine dump (debug=2)")
	cmd.Flags().BoolVar(&heap, "heap", false, "Capture a heap profile")
	cmd.Flags().BoolVar(&profile, "profile", false, "Capture a 10s CPU profile (opt-in; not part of the default set)")
	cmd.Flags().BoolVar(&extract, "extract", false, "Unpack dump bundle into output directory instead of writing a single .tar.gz")
	cmd.Flags().StringVarP(&output, "output", "o", "dumps", "Local directory to write the dump bundle into")
	cmd.Flags().StringVar(&maxSize, "max-size", "", "Fail if the dump exceeds this size (e.g. 2Gi); empty means unlimited")
	return cmd
}

func newDebugCmd(configFlags *genericclioptions.ConfigFlags, streams genericiooptions.IOStreams) *cobra.Command {
	o := &Options{configFlags: configFlags, IOStreams: streams}
	var shell string
	cmd := &cobra.Command{
		Use:     "debug <pod|deployment>",
		Aliases: []string{"sh"},
		Short:   "Open an interactive shell in a debug container beside the target",
		Long: `Drop into a shell in a toolbox container that shares the target's PID
namespace and runs as its matching UID, under a restricted profile that passes
admission — no need to remember the image, capabilities to drop, or --custom profile.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if err := o.Complete(c, args); err != nil {
				return err
			}
			if err := o.Validate(); err != nil {
				return err
			}
			return o.debug(c.Context(), []string{shell})
		},
	}
	o.addCaptureFlags(cmd, defaultToolboxImage)
	cmd.Flags().StringVar(&shell, "shell", "sh", "Shell to launch in the debug container")
	return cmd
}

func newEnvCmd(configFlags *genericclioptions.ConfigFlags, streams genericiooptions.IOStreams) *cobra.Command {
	o := &Options{configFlags: configFlags, IOStreams: streams}
	var noRedact bool
	cmd := &cobra.Command{
		Use:   "env <pod|deployment>",
		Short: "Capture the runtime environment of a pod's container",
		Long:  "Shortcut that reads /proc/1/environ from the target via a debug container.",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			o.redact = !noRedact
			return o.runCapture(c, args, envDumpCommand)
		},
	}
	o.addCaptureFlags(cmd, defaultToolboxImage)
	cmd.Flags().BoolVar(&noRedact, "no-redact", false, "Don't mask secret-looking env values")
	return cmd
}

func newInfoCmd(configFlags *genericclioptions.ConfigFlags, streams genericiooptions.IOStreams) *cobra.Command {
	o := &Options{configFlags: configFlags, IOStreams: streams}
	var noRedact bool
	var metricsPort, metricsPath string
	cmd := &cobra.Command{
		Use:   "info <pod|deployment>",
		Short: "Report the target process, its memory, cgroup limits and fd usage",
		Long: `Read what the target process is and what the kernel is enforcing on it —
command line, thread count, RSS/PSS, cgroup memory and CPU limits, open file
descriptors — from /proc and the target's own cgroup view. Needs no tooling in
the target image and no extra capabilities.

With --metrics-port, also scrape the app's metrics endpoint over localhost.
Sampling the same pod over time is what turns this into leak evidence.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			o.redact = !noRedact
			return o.runCapture(c, args, buildInfoCommand(metricsPort, metricsPath))
		},
	}
	o.addCaptureFlags(cmd, defaultToolboxImage)
	cmd.Flags().BoolVar(&noRedact, "no-redact", false, "Don't mask secret-looking values (the command line can carry them)")
	cmd.Flags().StringVar(&metricsPort, "metrics-port", "", "Also scrape the app's metrics endpoint on this localhost port")
	cmd.Flags().StringVar(&metricsPath, "metrics-path", "/metrics", "Path of the metrics endpoint")
	return cmd
}

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

With no step flags all three dumps are captured; naming any of --thread,
--histogram or --heap captures only those.

JFR profiling is not included yet.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			resolveSteps(c.Flags(), []string{"thread", "histogram", "heap"}, &thread, &histogram, &heap)
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
	cmd.Flags().BoolVar(&thread, "thread", false, "Capture a thread dump (jstack)")
	cmd.Flags().BoolVar(&histogram, "histogram", false, "Capture a GC class histogram (jcmd)")
	cmd.Flags().BoolVar(&heap, "heap", false, "Capture a heap dump (jmap)")
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
