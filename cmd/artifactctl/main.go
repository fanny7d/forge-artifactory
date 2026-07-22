package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"superfan.myasustor.com/fanchao/artifact-repository/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr, os.LookupEnv)
	if err == nil {
		return
	}
	_, _ = fmt.Fprintf(os.Stderr, "artifactctl: %v\n", err)
	if cli.IsUsageError(err) {
		_, _ = fmt.Fprintln(os.Stderr, cli.Usage())
		os.Exit(2)
	}
	os.Exit(1)
}
