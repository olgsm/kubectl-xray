package xray

import (
	"context"
	"fmt"

	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/kubectl/pkg/util/term"
)

// debug starts an interactive debug container beside the target — sharing its
// PID namespace and running as the matching UID under the restricted profile;
// then attaches the local terminal to it.
func (o *Options) debug(ctx context.Context, command []string) error {
	pod, err := o.resolvePod(ctx)
	if err != nil {
		return err
	}
	container := o.resolveContainer(pod)

	uid, gid, pod, err := o.resolveUID(ctx, pod, container)
	if err != nil {
		return err
	}

	ec, err := buildEphemeralContainer(container, o.image, command, true, true, uid, gid)
	if err != nil {
		return err
	}
	o.logf("starting debug container %s (image %s, UID %d) in %s/%s...", ec.Name, o.image, *uid, o.namespace, pod.Name)
	pod, err = injectEphemeralContainer(ctx, o.clientset, o.namespace, pod.Name, ec)
	if err != nil {
		return err
	}
	terminated, err := waitForEphemeralStart(ctx, o.clientset, o.namespace, pod.Name, ec.Name)
	if err != nil {
		return err
	}
	if terminated != nil {
		return fmt.Errorf("debug container %s exited before attach: %d (%s)", ec.Name, terminated.ExitCode, terminated.Reason)
	}

	return o.attachTTY(ctx, pod.Name, ec.Name)
}

// attachTTY puts the local terminal into raw mode and attaches stdin/stdout to
// the debug container, forwarding window-size changes for the session.
func (o *Options) attachTTY(ctx context.Context, podName, ecName string) error {
	t := term.TTY{In: o.In, Out: o.Out, Raw: true}
	if !t.IsTerminalIn() {
		return fmt.Errorf("debug needs an interactive terminal on stdin")
	}
	sizes := sizeQueueAdapter{t.MonitorSize(t.GetSize())}

	// Attaching to an already-running shell means its first prompt was printed
	// before this stream existed, so the screen looks idle until input arrives.
	// kubectl prints the same hint rather than faking input (which double-prints).
	o.logf("if you don't see a prompt, press enter")

	return t.Safe(func() error {
		return attachToContainer(ctx, o.restConfig, o.clientset, o.namespace, podName, ecName, o.In, o.Out, nil, true, sizes)
	})
}

// sizeQueueAdapter converts kubectl's term.TerminalSizeQueue to the
// remotecommand one the attach stream expects (identical fields, distinct types).
type sizeQueueAdapter struct{ delegate term.TerminalSizeQueue }

func (a sizeQueueAdapter) Next() *remotecommand.TerminalSize {
	s := a.delegate.Next()
	if s == nil {
		return nil
	}
	return &remotecommand.TerminalSize{Width: s.Width, Height: s.Height}
}
