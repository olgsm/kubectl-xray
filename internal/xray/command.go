package xray

import (
	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
)

// defaultToolboxImage is the debug image for env — small, has sh/tr.
const defaultToolboxImage = "busybox:1.36"

func (o *Options) addCaptureFlags(cmd *cobra.Command, defaultImage string) {
	cmd.Flags().StringVarP(&o.container, "container", "c", "", "Name of the container")
	cmd.Flags().StringVar(&o.image, "image", defaultImage, "Toolbox image for the debug container")
	cmd.Flags().Int64Var(&o.asUser, "run-as-user", 0, "Run the debug container as this UID (overrides the UID derived from the target)")
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
	root.AddCommand(newJVMDumpCmd(configFlags, streams))

	return root
}

func newEnvCmd(configFlags *genericclioptions.ConfigFlags, streams genericiooptions.IOStreams) *cobra.Command {
	o := &Options{configFlags: configFlags, IOStreams: streams}
	cmd := &cobra.Command{
		Use:   "env <pod|deployment>",
		Short: "Capture the runtime environment of a pod's container",
		Long:  "Shortcut that reads /proc/1/environ from the target via a debug container.",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return o.runCapture(c, args, envDumpCommand)
		},
	}
	o.addCaptureFlags(cmd, defaultToolboxImage)
	return cmd
}
