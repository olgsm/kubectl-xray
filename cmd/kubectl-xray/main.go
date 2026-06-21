package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/cli-runtime/pkg/genericiooptions"

	"kubectl-xray/internal/xray"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	streams := genericiooptions.IOStreams{
		In:     os.Stdin,
		Out:    os.Stdout,
		ErrOut: os.Stderr,
	}
	cmd := xray.NewCmdXRay(streams)

	if err := cmd.ExecuteContext(ctx); err != nil {
		_, _ = fmt.Fprintln(streams.ErrOut, "Error:", err)
		return 1
	}
	return 0
}
