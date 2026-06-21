package xray

import (
	"fmt"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
)

func (o *XRayOptions) addCaptureFlags(cmd *cobra.Command) {
	cmd.Flags().StringVarP(&o.container, "container", "c", "", "Name of the container")
	cmd.Flags().StringVar(&o.image, "image", "busybox:1.36", "Toolbox image for the debug container")
	cmd.Flags().Int64Var(&o.asUser, "run-as-user", 0, "Run the debug container as this UID (overrides the UID derived from the target)")
}

// newCaptureCmd wires the shared Complete/Validate/capture lifecycle for a mode.
func newCaptureCmd(o *XRayOptions, use, short string, command []string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if err := o.Complete(c, args); err != nil {
				return err
			}
			if err := o.Validate(); err != nil {
				return err
			}
			return o.capture(c.Context(), command)
		},
	}
	o.addCaptureFlags(cmd)
	return cmd
}

func NewCmdXRay(streams genericiooptions.IOStreams) *cobra.Command {
	configFlags := genericclioptions.NewConfigFlags(true)

	root := &cobra.Command{
		Use:           "kubectl-xray",
		Short:         "Inspect pod environments and capture execution dumps",
		SilenceUsage:  true, // don't dump usage on a runtime error
		SilenceErrors: true, // we print the error ourselves in main
	}
	root.CompletionOptions.DisableDefaultCmd = true // not useful for a kubectl plugin
	configFlags.AddFlags(root.PersistentFlags())

	// Each subcommand gets its own options sharing the persistent configFlags.
	envOpts := &XRayOptions{configFlags: configFlags, IOStreams: streams}
	root.AddCommand(newCaptureCmd(envOpts,
		"env <pod|deployment>",
		"Capture the runtime environment of a pod's container",
		envDumpCommand,
	))

	// TODO: dump command (JVM thread/heap, Go goroutine/pprof). Stubbed for now.
	root.AddCommand(&cobra.Command{
		Use:   "dump <pod|deployment>",
		Short: "Capture an execution dump from a pod's container (not yet implemented)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return fmt.Errorf("dump: not implemented yet")
		},
	})

	return root
}
