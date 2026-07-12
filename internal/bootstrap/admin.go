package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"superfan.myasustor.com/fanchao/artifact-repository/internal/audit"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/auth"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/clock"
	db "superfan.myasustor.com/fanchao/artifact-repository/internal/database/sqlc"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/id"
)

var ErrAlreadyBootstrapped = errors.New("an administrator has already been bootstrapped")

type AdminOptions struct {
	Pool     *pgxpool.Pool
	Name     string
	Pepper   []byte
	Random   io.Reader
	IDs      id.Generator
	Clock    clock.Clock
	Output   io.Writer
	TokenTTL time.Duration
}

type AdminResult struct {
	ServiceAccountID uuid.UUID
	TokenID          uuid.UUID
	Bearer           string
}

func BootstrapAdmin(ctx context.Context, options AdminOptions) (AdminResult, error) {
	if options.Pool == nil || options.Random == nil || options.IDs == nil || options.Clock == nil || options.Output == nil {
		return AdminResult{}, fmt.Errorf("bootstrap admin requires database, random, IDs, clock, and output")
	}
	if len(options.Pepper) != 32 {
		return AdminResult{}, fmt.Errorf("bootstrap admin pepper must be 32 bytes")
	}
	name := strings.TrimSpace(options.Name)
	if name == "" || len(name) > 128 {
		return AdminResult{}, fmt.Errorf("bootstrap admin name must contain 1 to 128 characters")
	}
	tokenTTL := options.TokenTTL
	if tokenTTL == 0 {
		tokenTTL = 365 * 24 * time.Hour
	}
	if tokenTTL <= 0 {
		return AdminResult{}, fmt.Errorf("bootstrap admin token TTL must be positive")
	}

	tx, err := options.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return AdminResult{}, fmt.Errorf("begin bootstrap admin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended('artifact-repository/bootstrap-admin', 0))"); err != nil {
		return AdminResult{}, fmt.Errorf("lock bootstrap admin initialization: %w", err)
	}
	count, err := db.New(tx).CountServiceAccounts(ctx)
	if err != nil {
		return AdminResult{}, fmt.Errorf("count service accounts: %w", err)
	}
	if count != 0 {
		return AdminResult{}, ErrAlreadyBootstrapped
	}
	account, err := db.New(tx).CreateServiceAccount(ctx, name)
	if err != nil {
		return AdminResult{}, fmt.Errorf("create bootstrap service account: %w", err)
	}
	tokenID := options.IDs.New()
	if tokenID == uuid.Nil {
		return AdminResult{}, fmt.Errorf("generate bootstrap token ID: nil UUID")
	}
	issued, err := auth.IssueToken(options.Random, tokenID, options.Pepper)
	if err != nil {
		return AdminResult{}, fmt.Errorf("issue bootstrap admin token: %w", err)
	}
	if _, err := db.New(tx).CreateAPIToken(ctx, db.CreateAPITokenParams{
		ID:               tokenID,
		ServiceAccountID: account.ID,
		SecretHmac:       issued.SecretHMAC,
		Scopes:           []string{"admin"},
		RepositoryIds:    []uuid.UUID{},
		ExpiresAt:        pgtype.Timestamptz{Time: options.Clock.Now().UTC().Add(tokenTTL), Valid: true},
	}); err != nil {
		return AdminResult{}, fmt.Errorf("persist bootstrap admin token: %w", err)
	}
	if _, err := audit.NewService(options.Pool).Record(ctx, tx, audit.Event{
		ActorTokenID: tokenID,
		Action:       "bootstrap-admin",
		ResourceType: "service_account",
		ResourceID:   account.ID.String(),
		Outcome:      audit.OutcomeSuccess,
		RequestID:    "bootstrap-admin",
		Details:      map[string]any{"serviceAccountId": account.ID.String()},
	}); err != nil {
		return AdminResult{}, fmt.Errorf("record bootstrap admin audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return AdminResult{}, fmt.Errorf("commit bootstrap admin transaction: %w", err)
	}
	if _, err := fmt.Fprintln(options.Output, issued.Bearer); err != nil {
		return AdminResult{}, fmt.Errorf("write bootstrap admin token: %w", err)
	}
	return AdminResult{ServiceAccountID: account.ID, TokenID: tokenID, Bearer: issued.Bearer}, nil
}
