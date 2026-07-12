package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

var (
	ErrUsage                = errors.New("usage: artifact-repository <api|worker|migrate|bootstrap-admin|keygen>")
	ErrCommandNotConfigured = errors.New("command is not configured")
)

type commandDependencies struct {
	api            func(context.Context) error
	worker         func(context.Context) error
	migrate        func(context.Context) error
	bootstrapAdmin func(context.Context, string) error
	keygen         func(context.Context, string, string) error
}

func Run(ctx context.Context, args []string) error {
	return run(ctx, args, defaultCommandDependencies())
}

func run(ctx context.Context, args []string, dependencies commandDependencies) error {
	if len(args) == 0 {
		return ErrUsage
	}
	switch args[0] {
	case "api":
		if len(args) != 1 {
			return ErrUsage
		}
		return callCommand(ctx, "api", dependencies.api)
	case "worker":
		if len(args) != 1 {
			return ErrUsage
		}
		return callCommand(ctx, "worker", dependencies.worker)
	case "migrate":
		if len(args) != 1 {
			return ErrUsage
		}
		return callCommand(ctx, "migrate", dependencies.migrate)
	case "bootstrap-admin":
		name, err := parseBootstrapAdmin(args[1:])
		if err != nil {
			return err
		}
		if dependencies.bootstrapAdmin == nil {
			return fmt.Errorf("%w: bootstrap-admin", ErrCommandNotConfigured)
		}
		return dependencies.bootstrapAdmin(ctx, name)
	case "keygen":
		privatePath, publicPath, err := parseKeygen(args[1:])
		if err != nil {
			return err
		}
		if dependencies.keygen == nil {
			return fmt.Errorf("%w: keygen", ErrCommandNotConfigured)
		}
		return dependencies.keygen(ctx, privatePath, publicPath)
	default:
		return ErrUsage
	}
}

func callCommand(ctx context.Context, name string, command func(context.Context) error) error {
	if command == nil {
		return fmt.Errorf("%w: %s", ErrCommandNotConfigured, name)
	}
	return command(ctx)
}

func parseBootstrapAdmin(args []string) (string, error) {
	flags := flag.NewFlagSet("bootstrap-admin", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	name := flags.String("name", "", "service account name")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || strings.TrimSpace(*name) == "" {
		return "", ErrUsage
	}
	return strings.TrimSpace(*name), nil
}

func parseKeygen(args []string) (string, string, error) {
	flags := flag.NewFlagSet("keygen", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	privatePath := flags.String("private-key-file", "", "private key output path")
	publicPath := flags.String("public-key-file", "", "public key output path")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || strings.TrimSpace(*privatePath) == "" || strings.TrimSpace(*publicPath) == "" {
		return "", "", ErrUsage
	}
	return strings.TrimSpace(*privatePath), strings.TrimSpace(*publicPath), nil
}
