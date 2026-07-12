package jobs

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	db "superfan.myasustor.com/fanchao/artifact-repository/internal/database/sqlc"
)

func (c *Cleaner) CleanupIdempotencyOnce(ctx context.Context) error {
	now := c.clock.Now().UTC()
	if _, err := db.New(c.pool).DeleteExpiredIdempotencyRecords(ctx, pgtype.Timestamptz{Time: now, Valid: true}); err != nil {
		return fmt.Errorf("delete expired idempotency records: %w", err)
	}
	return nil
}
