package jobs

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"superfan.myasustor.com/fanchao/artifact-repository/internal/clock"
	db "superfan.myasustor.com/fanchao/artifact-repository/internal/database/sqlc"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/release"
)

type PublishRecoverer interface {
	RecoverAttempt(context.Context, uuid.UUID) (release.PublishedRelease, error)
}

type PublishRecoveryOptions struct {
	Pool      *pgxpool.Pool
	Publisher PublishRecoverer
	Clock     clock.Clock
	BatchSize int32
}

type PublishRecovery struct {
	pool      *pgxpool.Pool
	publisher PublishRecoverer
	clock     clock.Clock
	batchSize int32
}

func NewPublishRecovery(options PublishRecoveryOptions) (*PublishRecovery, error) {
	if options.Pool == nil || options.Publisher == nil || options.Clock == nil {
		return nil, fmt.Errorf("publish recovery requires database, publisher, and clock dependencies")
	}
	if options.BatchSize <= 0 {
		return nil, fmt.Errorf("publish recovery batch size must be positive")
	}
	return &PublishRecovery{
		pool: options.Pool, publisher: options.Publisher, clock: options.Clock, batchSize: options.BatchSize,
	}, nil
}

func (r *PublishRecovery) RecoverPublishingOnce(ctx context.Context) error {
	now := r.clock.Now().UTC()
	attemptIDs, err := db.New(r.pool).ListRecoverablePublishAttemptIDs(ctx, db.ListRecoverablePublishAttemptIDsParams{
		Now:       pgtype.Timestamptz{Time: now, Valid: true},
		BatchSize: r.batchSize,
	})
	if err != nil {
		return fmt.Errorf("list recoverable publish attempts: %w", err)
	}
	for _, attemptID := range attemptIDs {
		if _, err := r.publisher.RecoverAttempt(ctx, attemptID); err != nil {
			if errors.Is(err, release.ErrLeaseLost) {
				continue
			}
			return fmt.Errorf("recover publish attempt %s: %w", attemptID, err)
		}
	}
	return nil
}
