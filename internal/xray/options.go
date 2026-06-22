package xray

import (
	"fmt"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type Options struct {
	configFlags *genericclioptions.ConfigFlags
	genericiooptions.IOStreams

	clientset  kubernetes.Interface
	restConfig *rest.Config

	namespace string
	target    string // pod or workload (deployment)
	container string
	image     string

	asUser       int64
	userOverride *int64 // set from --run-as-user when explicitly provided
}

func (o *Options) Complete(c *cobra.Command, args []string) error {
	ns, _, err := o.configFlags.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return err
	}
	o.namespace = ns
	o.target = args[0]

	if c.Flags().Changed("run-as-user") {
		o.userOverride = &o.asUser
	}

	o.restConfig, err = o.configFlags.ToRESTConfig()
	if err != nil {
		return err
	}
	o.clientset, err = kubernetes.NewForConfig(o.restConfig)
	return err
}

func (o *Options) Validate() error {
	if o.target == "" {
		return fmt.Errorf("pod or deployment name is required")
	}
	if o.image == "" {
		return fmt.Errorf("toolbox image is required (--image)")
	}
	return nil
}

// logf writes a progress line to stderr (ErrOut), keeping stdout clean for the
// captured payload so it stays pipeable.
func (o *Options) logf(format string, args ...any) {
	_, _ = fmt.Fprintf(o.ErrOut, "xray: "+format+"\n", args...)
}
