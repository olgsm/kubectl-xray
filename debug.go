package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

// probeUID runs the discovery container as an arbitrary nonroot user; it only
// reads world-readable /proc/1/status, so its own UID is irrelevant.
const probeUID int64 = 65534 // "nobody"

// Profile is the "how it's allowed to run" half, decoupled from "what to run".
type Profile struct {
	Name            string
	SecurityContext *corev1.SecurityContext
}

// deriveUser returns the UID/GID the target runs as, so the debug container can
// match it — the caps-free key to /proc/<pid>/environ reads and JVM/Go attach.
//
// Precedence: target container SecurityContext -> pod SecurityContext -> nil.
func deriveUser(pod *corev1.Pod, targetContainer string) (uid, gid *int64) {
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		if c.Name != targetContainer {
			continue
		}
		if sc := c.SecurityContext; sc != nil {
			uid, gid = copyInt64(sc.RunAsUser), copyInt64(sc.RunAsGroup)
		}
		break
	}
	// Pod-level fills whatever the container left unset.
	if psc := pod.Spec.SecurityContext; psc != nil {
		if uid == nil {
			uid = copyInt64(psc.RunAsUser)
		}
		if gid == nil {
			gid = copyInt64(psc.RunAsGroup)
		}
	}
	return uid, gid
}

// copyInt64 dereferences and re-pointers so callers can't mutate the pod's spec.
func copyInt64(p *int64) *int64 {
	if p == nil {
		return nil
	}
	v := *p
	return new(v)
}

// randSuffix yields a short hex suffix so repeated runs don't collide on name.
func randSuffix() (string, error) {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// discoverTargetUID injects a throwaway probe container into the target's PID
// namespace and reads the effective UID of PID 1 from world-readable
// /proc/1/status — the real running UID, which we then match to read environ.
// It returns the updated pod (the probe is appended to its ephemeral list).
func discoverTargetUID(ctx context.Context, client kubernetes.Interface, namespace string, pod *corev1.Pod, targetContainer, image string) (*int64, *corev1.Pod, error) {
	pu := probeUID
	ec, err := buildEphemeralContainer(targetContainer, image, []string{"cat", "/proc/1/status"}, false, &pu, nil)
	if err != nil {
		return nil, nil, err
	}
	pod, err = injectEphemeralContainer(ctx, client, namespace, pod.Name, ec)
	if err != nil {
		return nil, nil, fmt.Errorf("uid probe: %w", err)
	}
	term, err := waitForEphemeralStart(ctx, client, namespace, pod.Name, ec.Name)
	if err != nil {
		return nil, nil, err
	}
	var buf bytes.Buffer
	if err := streamEphemeralLogs(ctx, client, namespace, pod.Name, ec.Name, &buf); err != nil {
		return nil, nil, err
	}
	if term != nil && term.ExitCode != 0 {
		return nil, nil, fmt.Errorf("uid probe exited %d: %s", term.ExitCode, strings.TrimSpace(buf.String()))
	}
	uid, err := parseStatusUID(buf.String())
	if err != nil {
		return nil, nil, err
	}
	return &uid, pod, nil
}

// parseStatusUID extracts the effective UID from a /proc/<pid>/status dump.
// The "Uid:" line is: real  effective  saved  fs.
func parseStatusUID(status string) (int64, error) {
	for _, line := range strings.Split(status, "\n") {
		if !strings.HasPrefix(line, "Uid:") {
			continue
		}
		f := strings.Fields(line)
		if len(f) >= 2 {
			return strconv.ParseInt(f[1], 10, 64)
		}
	}
	return 0, fmt.Errorf("no Uid line in /proc/1/status")
}

// buildEphemeralContainer assembles the debug container spec under the restricted
// profile, running as the already-resolved uid/gid.
func buildEphemeralContainer(targetContainer, image string, command []string, tty bool, uid, gid *int64) (corev1.EphemeralContainer, error) {
	suffix, err := randSuffix()
	if err != nil {
		return corev1.EphemeralContainer{}, fmt.Errorf("generating container name: %w", err)
	}

	ec := corev1.EphemeralContainer{
		EphemeralContainerCommon: corev1.EphemeralContainerCommon{
			Name:    "xray-debug-" + suffix,
			Image:   image,
			Stdin:   tty,
			TTY:     tty,
			Command: command,
			SecurityContext: &corev1.SecurityContext{
				RunAsNonRoot:             new(true),
				AllowPrivilegeEscalation: new(false),
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				RunAsUser:                uid,
				RunAsGroup:               gid,
				SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
		},
		TargetContainerName: targetContainer, // PID-namespace join
	}

	// TODO: apply named profile, then strategic-merge the --custom overlay
	//       (strategicpatch.StrategicMergePatch on the marshaled container).
	return ec, nil
}

// injectEphemeralContainer adds the container via the ephemeralcontainers
// subresource (the only mutation path). It fetches the latest pod, appends, and
// updates under RetryOnConflict, since concurrent status changes (e.g. a prior
// probe container terminating) bump the pod's resourceVersion between calls.
func injectEphemeralContainer(ctx context.Context, client kubernetes.Interface, namespace, podName string, ec corev1.EphemeralContainer) (*corev1.Pod, error) {
	pods := client.CoreV1().Pods(namespace)

	var updated *corev1.Pod
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		pod, err := pods.Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		pod.Spec.EphemeralContainers = append(pod.Spec.EphemeralContainers, ec)
		updated, err = pods.UpdateEphemeralContainers(ctx, podName, pod, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("injecting ephemeral container: %w", err)
	}
	return updated, nil
}

// waitForEphemeralStart blocks until the named ephemeral container has started:
// it returns once the container is Running, or its terminal state if it already
// finished (a fast one-shot command can exit before we observe Running). The
// caller streams logs in both cases and inspects the returned exit code.
func waitForEphemeralStart(ctx context.Context, client kubernetes.Interface, namespace, podName, ecName string) (*corev1.ContainerStateTerminated, error) {
	sel := fields.OneTermEqualSelector("metadata.name", podName).String()
	w, err := client.CoreV1().Pods(namespace).Watch(ctx, metav1.ListOptions{FieldSelector: sel})
	if err != nil {
		return nil, fmt.Errorf("watching pod %s/%s: %w", namespace, podName, err)
	}
	defer w.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case ev, ok := <-w.ResultChan():
			if !ok {
				return nil, fmt.Errorf("watch closed before %s started", ecName)
			}
			pod, ok := ev.Object.(*corev1.Pod)
			if !ok {
				continue
			}
			for _, st := range pod.Status.EphemeralContainerStatuses {
				if st.Name != ecName {
					continue
				}
				switch {
				case st.State.Running != nil:
					return nil, nil
				case st.State.Terminated != nil:
					return st.State.Terminated, nil
				}
			}
		}
	}
}

// streamEphemeralLogs follows the container's stdout into out, capturing it
// client-side (the evidence the k8s API drops once the session ends).
func streamEphemeralLogs(ctx context.Context, client kubernetes.Interface, namespace, podName, ecName string, out io.Writer) error {
	req := client.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
		Container: ecName,
		Follow:    true,
	})
	rc, err := req.Stream(ctx)
	if err != nil {
		return fmt.Errorf("streaming logs for %s: %w", ecName, err)
	}
	defer func() { _ = rc.Close() }() // read side: Close error is not meaningful

	_, err = io.Copy(out, rc)
	return err
}
