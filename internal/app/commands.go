package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"superfan.myasustor.com/fanchao/artifact-repository/internal/bootstrap"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/clock"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/config"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/database"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/id"
)

type commandEnvironment struct {
	lookup config.LookupFunc
	output io.Writer
	random io.Reader
}

func defaultCommandDependencies() commandDependencies {
	return newCommandDependencies(commandEnvironment{
		lookup: os.LookupEnv,
		output: os.Stdout,
		random: rand.Reader,
	})
}

func newCommandDependencies(environment commandEnvironment) commandDependencies {
	lookup := environment.lookup
	if lookup == nil {
		lookup = os.LookupEnv
	}
	output := environment.output
	if output == nil {
		output = io.Discard
	}
	random := environment.random
	if random == nil {
		random = rand.Reader
	}
	return commandDependencies{
		api: func(ctx context.Context) error {
			cfg, err := config.Load(lookup)
			if err != nil {
				return err
			}
			return runAPI(ctx, cfg, random)
		},
		worker: func(ctx context.Context) error {
			cfg, err := config.Load(lookup)
			if err != nil {
				return err
			}
			return runWorker(ctx, cfg, random)
		},
		migrate: func(ctx context.Context) error {
			databaseURL, err := requiredEnvironment(lookup, "DATABASE_URL")
			if err != nil {
				return err
			}
			return database.RunMigrateCommand(ctx, databaseURL)
		},
		bootstrapAdmin: func(ctx context.Context, name string) error {
			return runBootstrapAdmin(ctx, lookup, output, random, name)
		},
		keygen: func(ctx context.Context, privatePath, publicPath string) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			info, err := bootstrap.GenerateKeyPair(privatePath, publicPath, random)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(output, "keyId=%s\nfingerprint=%s\n", info.KeyID, info.Fingerprint); err != nil {
				return fmt.Errorf("write generated key identity: %w", err)
			}
			return nil
		},
	}
}

func runBootstrapAdmin(ctx context.Context, lookup config.LookupFunc, output io.Writer, random io.Reader, name string) error {
	databaseURL, err := requiredEnvironment(lookup, "DATABASE_URL")
	if err != nil {
		return err
	}
	pepper, err := environmentKey(lookup, "TOKEN_PEPPER")
	if err != nil {
		return err
	}
	pool, err := openCheckedDatabase(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	_, err = bootstrap.BootstrapAdmin(ctx, bootstrap.AdminOptions{
		Pool: pool, Name: name, Pepper: pepper, Random: random,
		IDs: id.UUIDGenerator{}, Clock: clock.System{}, Output: output,
		TokenTTL: 365 * 24 * time.Hour,
	})
	return err
}

func requiredEnvironment(lookup config.LookupFunc, name string) (string, error) {
	value, ok := lookup(name)
	if !ok || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return strings.TrimSpace(value), nil
}

func environmentKey(lookup config.LookupFunc, name string) ([]byte, error) {
	encoded, err := requiredEnvironment(lookup, name)
	if err != nil {
		return nil, err
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("%s must be base64url without padding: %w", name, err)
	}
	if len(decoded) != 32 {
		return nil, fmt.Errorf("%s must decode to 32 bytes", name)
	}
	return decoded, nil
}
