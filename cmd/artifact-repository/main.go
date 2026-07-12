package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"superfan.myasustor.com/fanchao/artifact-repository/internal/app"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := app.Run(ctx, os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}
