package xray

import (
	"context"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"kubectl-xray/internal/redact"
)

const defaultContainerAnnotation = "kubectl.kubernetes.io/default-container"

// envDumpCommand reads the target's runtime environment from /proc without
// relying on any binary in the target image — only the toolbox needs sh/tr.
// Redirection (not cat|tr) so a permission failure yields a non-zero exit
// instead of being masked by tr's success at the end of a pipe.
var envDumpCommand = []string{"sh", "-c", "tr '\\0' '\\n' < /proc/1/environ"}

// resolvePod accepts either a pod name or a deployment name, returning a
// concrete pod to target (a Running one when resolving via deployment).
func (o *Options) resolvePod(ctx context.Context) (*corev1.Pod, error) {
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
func (o *Options) resolveContainer(pod *corev1.Pod) string {
	if o.container != "" {
		return o.container
	}
	if name := pod.Annotations[defaultContainerAnnotation]; name != "" {
		return name
	}
	return pod.Spec.Containers[0].Name
}

// resolveUID matches the target's UID (spec -> --run-as-user -> runtime probe).
// It returns the possibly-updated pod (the probe appends an ephemeral container).
func (o *Options) resolveUID(ctx context.Context, pod *corev1.Pod, container string) (uid, gid *int64, updated *corev1.Pod, err error) {
	uid, gid = deriveUser(pod, container)
	if o.userOverride != nil {
		uid = o.userOverride
	}
	if uid == nil {
		o.logf("%s/%s container %q has no runAsUser; probing PID 1 to discover it...", o.namespace, pod.Name, container)
		uid, pod, err = discoverTargetUID(ctx, o.clientset, o.namespace, pod, container, o.image)
		if err != nil {
			return nil, nil, nil, err
		}
		o.logf("discovered UID %d", *uid)
	}
	return uid, gid, pod, nil
}

// capture runs command in an ephemeral toolbox container alongside the target
// and streams its stdout to o.Out via the container logs. Used by env.
func (o *Options) capture(ctx context.Context, command []string) error {
	pod, err := o.resolvePod(ctx)
	if err != nil {
		return err
	}
	container := o.resolveContainer(pod)

	uid, gid, pod, err := o.resolveUID(ctx, pod, container)
	if err != nil {
		return err
	}

	ec, err := buildEphemeralContainer(container, o.image, command, false, false, uid, gid)
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
	out := io.Writer(o.Out)
	var rw *redact.Writer
	if o.redact {
		rw = redact.NewWriter(o.Out)
		out = rw
	}
	if err := streamEphemeralLogs(ctx, o.clientset, o.namespace, pod.Name, ec.Name, out); err != nil {
		return err
	}
	if rw != nil {
		if err := rw.Flush(); err != nil {
			return err
		}
		if rw.N > 0 {
			o.logf("masked %d secret-looking value(s); use --no-redact to disable", rw.N)
		}
	}
	if term != nil && term.ExitCode != 0 {
		return fmt.Errorf("%s exited %d (%s)", ec.Name, term.ExitCode, term.Reason)
	}

	return nil
}
