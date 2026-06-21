package xray

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/kubernetes"
)

type XRayOptions struct {
	configFlags *genericclioptions.ConfigFlags
	genericiooptions.IOStreams

	clientset kubernetes.Interface

	namespace string
	target    string // pod or workload (deployment) name from args
	container string
	image     string

	asUser       int64
	userOverride *int64 // set from --run-as-user when explicitly provided
}

func (o *XRayOptions) Complete(c *cobra.Command, args []string) error {
	ns, _, err := o.configFlags.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return err
	}
	o.namespace = ns
	o.target = args[0]

	if c.Flags().Changed("run-as-user") {
		o.userOverride = &o.asUser
	}

	restConfig, err := o.configFlags.ToRESTConfig()
	if err != nil {
		return err
	}
	o.clientset, err = kubernetes.NewForConfig(restConfig)
	return err
}

func (o *XRayOptions) Validate() error {
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
func (o *XRayOptions) logf(format string, args ...any) {
	fmt.Fprintf(o.ErrOut, "xray: "+format+"\n", args...)
}

const defaultContainerAnnotation = "kubectl.kubernetes.io/default-container"

// envDumpCommand reads the target's runtime environment from /proc without
// relying on any binary in the target image — only the toolbox needs sh/tr.
// Redirection (not cat|tr) so a permission failure yields a non-zero exit
// instead of being masked by tr's success at the end of a pipe.
var envDumpCommand = []string{"sh", "-c", "tr '\\0' '\\n' < /proc/1/environ"}

// resolvePod accepts either a pod name or a deployment name, returning a
// concrete pod to target (a Running one when resolving via deployment).
func (o *XRayOptions) resolvePod(ctx context.Context) (*corev1.Pod, error) {
	pods := o.clientset.CoreV1().Pods(o.namespace)

	pod, err := pods.Get(ctx, o.target, metav1.GetOptions{})
	if err == nil {
		return pod, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("getting pod %s/%s: %w", o.namespace, o.target, err)
	}

	// Not a pod — try a deployment of that name.
	dep, derr := o.clientset.AppsV1().Deployments(o.namespace).Get(ctx, o.target, metav1.GetOptions{})
	if derr != nil {
		return nil, fmt.Errorf("no pod or deployment named %q in namespace %s", o.target, o.namespace)
	}
	sel, serr := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if serr != nil {
		return nil, fmt.Errorf("building selector for deployment %s: %w", o.target, serr)
	}
	list, lerr := pods.List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if lerr != nil {
		return nil, fmt.Errorf("listing pods for deployment %s: %w", o.target, lerr)
	}
	return pickPod(list.Items, o.target)
}

// pickPod prefers a Running pod, falling back to the first one.
func pickPod(pods []corev1.Pod, deployment string) (*corev1.Pod, error) {
	if len(pods) == 0 {
		return nil, fmt.Errorf("deployment %q has no pods", deployment)
	}
	for i := range pods {
		if pods[i].Status.Phase == corev1.PodRunning {
			return &pods[i], nil
		}
	}
	return &pods[0], nil
}

// resolveContainer applies the -c flag, the default-container annotation, then
// falls back to the first container.
func (o *XRayOptions) resolveContainer(pod *corev1.Pod) string {
	if o.container != "" {
		return o.container
	}
	if name := pod.Annotations[defaultContainerAnnotation]; name != "" {
		return name
	}
	return pod.Spec.Containers[0].Name
}

// capture runs command in an ephemeral toolbox container alongside the target
// and streams its output to o.Out. Shared by env/dump/etc.
func (o *XRayOptions) capture(ctx context.Context, command []string) error {
	pod, err := o.resolvePod(ctx)
	if err != nil {
		return err
	}
	container := o.resolveContainer(pod)

	// Match the target's UID: spec -> --run-as-user -> runtime /proc probe.
	uid, gid := deriveUser(pod, container)
	if o.userOverride != nil {
		uid = o.userOverride
	}
	if uid == nil {
		o.logf("%s/%s container %q has no runAsUser; probing PID 1 to discover it...", o.namespace, pod.Name, container)
		uid, pod, err = discoverTargetUID(ctx, o.clientset, o.namespace, pod, container, o.image)
		if err != nil {
			return err
		}
		o.logf("discovered UID %d; injecting debug container as that UID...", *uid)
	}

	ec, err := buildEphemeralContainer(container, o.image, command, false, uid, gid)
	if err != nil {
		return err
	}
	pod, err = injectEphemeralContainer(ctx, o.clientset, o.namespace, pod.Name, ec)
	if err != nil {
		return err
	}
	term, err := waitForEphemeralStart(ctx, o.clientset, o.namespace, pod.Name, ec.Name)
	if err != nil {
		return err
	}
	// Stream logs whether it's still running or already finished — the output
	// is the captured evidence, readable in both cases.
	if err := streamEphemeralLogs(ctx, o.clientset, o.namespace, pod.Name, ec.Name, o.Out); err != nil {
		return err
	}
	if term != nil && term.ExitCode != 0 {
		return fmt.Errorf("%s exited %d (%s)", ec.Name, term.ExitCode, term.Reason)
	}
	return nil
}

func (o *XRayOptions) addCaptureFlags(cmd *cobra.Command) {
	cmd.Flags().StringVarP(&o.container, "container", "c", "", "Name of the container")
	cmd.Flags().StringVar(&o.image, "image", "busybox:1.36", "Toolbox image for the debug container")
	cmd.Flags().Int64Var(&o.asUser, "run-as-user", 0, "Run the debug container as this UID (overrides the UID derived from the target)")
}

// newCaptureCmd wires the shared Complete/Validate lifecycle for a capture mode.
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
