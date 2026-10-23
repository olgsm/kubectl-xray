package xray

import (
	"context"
	"errors"
	"fmt"

	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
	"k8s.io/kubectl/pkg/util/term"
)

// debug starts a toolbox beside the target — sharing its PID namespace and
// running as the matching UID under the restricted profile — and runs an
// interactive shell in it on the local terminal.
func (o *Options) debug(ctx context.Context, command []string) error {
	pod, err := o.resolvePod(ctx)
	if err != nil {
		return err
	}
	t := term.TTY{In: o.In, Out: o.Out, Raw: true}
	if !t.IsTerminalIn() {
		return fmt.Errorf("debug needs an interactive terminal on stdin")
	}
	pod, ecName, err := o.startToolbox(ctx, pod, "debug")
	if err != nil {
		return err
	}
	sizes := sizeQueueAdapter{t.MonitorSize(t.GetSize())}
	err = t.Safe(func() error {
		return execInContainer(ctx, o.restConfig, o.clientset, o.namespace, pod.Name, ecName, inToolbox(command...), o.In, o.Out, nil, true, sizes)
	})
	// The shell's exit status is the user's own, not a failure of the session.
	var exitErr utilexec.ExitError
	if errors.As(err, &exitErr) {
		return nil
	}
	return err
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
