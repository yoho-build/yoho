package cli

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// signalContext cancels on the first SIGINT/SIGTERM so cleanups (unlock,
// unpause, removing a failed new container) still run; a second signal
// exits immediately.
func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		cancel()
		<-ch
		os.Exit(130)
	}()
	return ctx, func() { signal.Stop(ch); cancel() }
}
