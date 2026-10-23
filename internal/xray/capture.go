package xray

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilexec "k8s.io/client-go/util/exec"

	"github.com/olgsm/kubectl-xray/internal/redact"
)

const defaultContainerAnnotation = "kubectl.kubernetes.io/default-container"

// envDumpCommand reads the target's runtime environment from /proc without
// relying on any binary in the target image — only the toolbox needs sh/tr.
// Redirection (not cat|tr) so a permission failure yields a non-zero exit
// instead of being masked by tr's success at the end of a pipe.
var envDumpCommand = []string{"sh", "-c", "tr '\\0' '\\n' < /proc/1/environ"}

// parseTarget splits kubectl's TYPE/NAME form. An empty kind means the argument
// was a bare name, which resolves as a pod first and then as a deployment.
func parseTarget(arg string) (kind, name string, err error) {
	k, n, ok := strings.Cut(arg, "/")
	if !ok {
		return "", arg, nil
	}
	if n == "" {
		return "", "", fmt.Errorf("no resource name in %q", arg)
	}
	switch k {
	case "pod", "pods", "po":
		return "pod", n, nil
	case "deployment", "deployments", "deploy":
		return "deployment", n, nil
	}
	return "", "", fmt.Errorf("unsupported resource type %q: use pod/NAME or deployment/NAME", k)
}

// resolvePod turns the target into a concrete pod, reporting which pod it chose
// whenever a deployment left it a choice.
func (o *Options) resolvePod(ctx context.Context) (*corev1.Pod, error) {
	pods := o.clientset.CoreV1().Pods(o.namespace)

	if o.kind != "deployment" {
		pod, err := pods.Get(ctx, o.target, metav1.GetOptions{})
		if err == nil {
			return pod, nil
		}
		if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("getting pod %s/%s: %w", o.namespace, o.target, err)
		}
		if o.kind == "pod" {
			return nil, fmt.Errorf("no pod named %q in namespace %s", o.target, o.namespace)
		}
	}

	dep, derr := o.clientset.AppsV1().Deployments(o.namespace).Get(ctx, o.target, metav1.GetOptions{})
	if derr != nil {
		if o.kind == "deployment" {
			return nil, fmt.Errorf("no deployment named %q in namespace %s", o.target, o.namespace)
		}
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
	pod := pickPod(list.Items)
	if pod == nil {
		return nil, fmt.Errorf("deployment %q has no pods", o.target)
	}
	if len(list.Items) > 1 {
		o.logf("found %d pods for deployment %s, using pod/%s", len(list.Items), o.target, pod.Name)
	}
	return pod, nil
}

// pickPod mirrors kubectl's choice for a workload target: a ready Running pod,
// else any Running one, else anything — newest first among equals.
func pickPod(pods []corev1.Pod) *corev1.Pod {
	var best *corev1.Pod
	for i := range pods {
		p := &pods[i]
		switch {
		case best == nil,
			podRank(p) > podRank(best),
			podRank(p) == podRank(best) && p.CreationTimestamp.After(best.CreationTimestamp.Time):
			best = p
		}
	}
	return best
}

func podRank(p *corev1.Pod) int {
	if p.Status.Phase != corev1.PodRunning {
		return 0
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return 2
		}
	}
	return 1
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

// startToolbox injects an idle toolbox (idleCommand) beside the target, matched
// to its UID, and waits for it to run. Work is then exec'd into it via
// inToolbox, so none of it lands in the container log.
func (o *Options) startToolbox(ctx context.Context, pod *corev1.Pod, label string) (*corev1.Pod, string, error) {
	container := o.resolveContainer(pod)
	uid, gid, pod, err := o.resolveUID(ctx, pod, container)
	if err != nil {
		return nil, "", err
	}
	ec, err := buildEphemeralContainer(container, o.image, idleCommand(startTimeout), uid, gid)
	if err != nil {
		return nil, "", err
	}
	o.logf("injecting %s toolbox %s (image %s, UID %d) in %s/%s...", label, ec.Name, o.image, *uid, o.namespace, pod.Name)
	pod, err = injectEphemeralContainer(ctx, o.clientset, o.namespace, pod.Name, ec)
	if err != nil {
		return nil, "", err
	}
	term, err := waitForEphemeralStart(ctx, o.clientset, o.namespace, pod.Name, ec.Name)
	if err != nil {
		return nil, "", err
	}
	if term != nil {
		return nil, "", fmt.Errorf("toolbox %s exited before it could run anything: %d (%s)", ec.Name, term.ExitCode, term.Reason)
	}
	return pod, ec.Name, nil
}

// capture runs command in a toolbox beside the target and writes its stdout to
// o.Out. Used by env and info.
func (o *Options) capture(ctx context.Context, label string, command []string) error {
	pod, err := o.resolvePod(ctx)
	if err != nil {
		return err
	}
	return o.captureOn(ctx, pod, label, command)
}

// captureOn is capture against an already-resolved pod, so a caller that needs
// the pod's identity first doesn't resolve it twice (and risk landing on a
// different replica the second time).
func (o *Options) captureOn(ctx context.Context, pod *corev1.Pod, label string, command []string) error {
	pod, ecName, err := o.startToolbox(ctx, pod, label)
	if err != nil {
		return err
	}

	out := o.Out
	var rw *redact.Writer
	if o.redact {
		rw = redact.NewWriter(o.Out)
		out = rw
	}
	err = execInContainer(ctx, o.restConfig, o.clientset, o.namespace, pod.Name, ecName, inToolbox(command...), nil, out, o.ErrOut, false, nil)
	if rw != nil {
		if ferr := rw.Flush(); ferr != nil {
			return ferr
		}
		if rw.N > 0 {
			o.logf("masked %d secret-looking value(s); use --no-redact to disable", rw.N)
		}
	}
	var exitErr utilexec.ExitError
	if errors.As(err, &exitErr) {
		return fmt.Errorf("%s exited %d", ecName, exitErr.ExitStatus())
	}
	return err
}
