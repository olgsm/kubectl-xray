package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

type XRayOptions struct {
	configFlags *genericclioptions.ConfigFlags
	genericiooptions.IOStreams

	restConfig *rest.Config
	clientset  kubernetes.Interface

	namespace string
	pod       string
	container string
}

func (o *XRayOptions) Complete(c *cobra.Command, args []string) error {
	ns, _, err := o.configFlags.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return err
	}
	o.namespace = ns
	o.pod = args[0]

	o.restConfig, err = o.configFlags.ToRESTConfig()
	if err != nil {
		return err
	}
	o.clientset, err = kubernetes.NewForConfig(o.restConfig)
	if err != nil {
		return err
	}
	return nil
}

func (o *XRayOptions) Validate() error {
	if o.pod == "" {
		return fmt.Errorf("pod name is required")
	}
	return nil
}

// defaultContainerAnnotation lets a pod nominate its primary container,
// matching kubectl's container-selection behavior.
const defaultContainerAnnotation = "kubectl.kubernetes.io/default-container"

func (o *XRayOptions) Run(ctx context.Context) error {
	pod, err := o.clientset.CoreV1().Pods(o.namespace).Get(ctx, o.pod, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting pod %s/%s: %w", o.namespace, o.pod, err)
	}

	container := o.container
	if container == "" {
		if name := pod.Annotations[defaultContainerAnnotation]; name != "" {
			container = name
		} else {
			container = pod.Spec.Containers[0].Name
		}
	}

	req := o.clientset.CoreV1().RESTClient().
		Post().
		Resource("pods").
		Namespace(o.namespace).
		Name(o.pod).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   []string{"env"},
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	// WebSocket (GET) with SPDY (POST) fallback for older clusters.
	wsExec, err := remotecommand.NewWebSocketExecutor(o.restConfig, "GET", req.URL().String())
	if err != nil {
		return fmt.Errorf("creating websocket executor: %w", err)
	}
	spdyExec, err := remotecommand.NewSPDYExecutor(o.restConfig, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("creating spdy executor: %w", err)
	}
	exec, err := remotecommand.NewFallbackExecutor(wsExec, spdyExec, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
	if err != nil {
		return fmt.Errorf("creating fallback executor: %w", err)
	}

	return exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: o.Out,
		Stderr: o.ErrOut,
	})
}

func NewXRayOptions(streams genericiooptions.IOStreams) *XRayOptions {
	return &XRayOptions{
		configFlags: genericclioptions.NewConfigFlags(true),
		IOStreams:   streams,
	}
}

func NewCmdXRay(streams genericiooptions.IOStreams) *cobra.Command {
	o := NewXRayOptions(streams)

	cmd := &cobra.Command{
		Use:   "kubectl-xray [pod] [flags]",
		Short: "Inspect pod environments and capture execution dumps",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if err := o.Complete(c, args); err != nil {
				return err
			}
			if err := o.Validate(); err != nil {
				return err
			}
			return o.Run(c.Context())
		},
	}

	o.configFlags.AddFlags(cmd.Flags())
	cmd.Flags().StringVarP(&o.container, "container", "c", "", "Name of the container")

	return cmd
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	streams := genericiooptions.IOStreams{
		In:     os.Stdin,
		Out:    os.Stdout,
		ErrOut: os.Stderr,
	}
	cmd := NewCmdXRay(streams)

	if err := cmd.ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}
