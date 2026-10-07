package main

import (
	"context"
	"gitlocal/internal/cli"
	"os"
	"os/signal"
)

func main() {
	ctx, stop := cancelContext()
	defer stop()
	os.Exit(cli.Execute(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// cancelContext is canceled by the first interrupt or termination signal.
// Later signals get their default action again, so a second one ends a
// process whose cancellation hangs.
func cancelContext() (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), cancelSignals...)
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ctx, stop
}
