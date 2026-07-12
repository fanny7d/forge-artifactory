package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	db "superfan.myasustor.com/fanchao/artifact-repository/internal/database/sqlc"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/storage"
)

func (c *Cleaner) CleanupUploadsOnce(ctx context.Context) error {
	queries := db.New(c.pool)
	var cleanupErrors []error
	for count := int32(0); count < c.batchSize; count++ {
		now := c.clock.Now().UTC()
		owner := c.ids.New()
		if owner == uuid.Nil {
			return fmt.Errorf("generate upload cleanup owner: nil UUID")
		}
		session, err := queries.ClaimExpiredUploadSession(ctx, db.ClaimExpiredUploadSessionParams{
			LeaseOwner:     owner,
			LeaseExpiresAt: pgtype.Timestamptz{Time: now.Add(c.deleteLease), Valid: true},
			Now:            pgtype.Timestamptz{Time: now, Valid: true},
		})
		if errors.Is(err, pgx.ErrNoRows) {
			cleanupErrors = append(cleanupErrors, c.reconcileStorageStagingObjects(ctx, int(c.batchSize-count)))
			return errors.Join(cleanupErrors...)
		}
		if err != nil {
			return fmt.Errorf("claim expired upload session: %w", err)
		}
		if err := c.store.Delete(ctx, session.StagingKey); err != nil && !errors.Is(err, storage.ErrNotFound) {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("delete expired upload staging object %q: %w", session.StagingKey, err))
			continue
		}
		rows, err := queries.CompleteUploadCleanup(ctx, db.CompleteUploadCleanupParams{
			ID:                 session.ID,
			LeaseOwner:         owner,
			LeaseGeneration:    session.LeaseGeneration,
			CleanupCompletedAt: pgtype.Timestamptz{Time: now, Valid: true},
		})
		if err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("complete expired upload cleanup %s: %w", session.ID, err))
			continue
		}
		if rows != 1 {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("complete expired upload cleanup %s: lease lost", session.ID))
		}
	}
	return errors.Join(cleanupErrors...)
}

func (c *Cleaner) reconcileStorageStagingObjects(ctx context.Context, limit int) error {
	if limit <= 0 {
		return nil
	}
	c.listMu.Lock()
	defer c.listMu.Unlock()

	page, err := c.store.List(ctx, storage.ListRequest{
		Prefix: "staging/",
		After:  c.stagingListAfter,
		Limit:  limit,
	})
	if err != nil {
		return fmt.Errorf("list storage staging objects: %w", err)
	}
	c.stagingListAfter = page.NextAfter

	cutoff := c.clock.Now().UTC().Add(-c.orphanRetention)
	var cleanupErrors []error
	for _, object := range page.Items {
		if object.LastModified.IsZero() || !object.LastModified.Before(cutoff) {
			continue
		}
		protected, recognized, err := c.stagingObjectProtected(ctx, object.Key)
		if err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("check staging object owner %q: %w", object.Key, err))
			continue
		}
		if !recognized || protected {
			continue
		}
		operationContext, cancel := context.WithTimeout(ctx, c.deleteTimeout)
		err = c.store.Delete(operationContext, object.Key)
		cancel()
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("delete rowless staging object %q: %w", object.Key, err))
		}
	}
	return errors.Join(cleanupErrors...)
}

func (c *Cleaner) stagingObjectProtected(ctx context.Context, key string) (protected, recognized bool, err error) {
	parts := strings.Split(key, "/")
	queries := db.New(c.pool)
	if len(parts) == 3 && parts[0] == "staging" && parts[1] == "uploads" {
		if _, parseErr := uuid.Parse(parts[2]); parseErr != nil {
			return false, false, nil
		}
		protected, err := queries.HasPendingUploadSessionForStagingKey(ctx, key)
		return protected, true, err
	}
	if len(parts) == 4 && parts[0] == "staging" && parts[1] == "publish" && (parts[3] == "manifest" || parts[3] == "signature") {
		attemptID, parseErr := uuid.Parse(parts[2])
		if parseErr != nil {
			return false, false, nil
		}
		attempt, getErr := queries.GetPublishAttempt(ctx, attemptID)
		if errors.Is(getErr, pgx.ErrNoRows) {
			return false, true, nil
		}
		if getErr != nil {
			return false, true, getErr
		}
		return attempt.State == "active", true, nil
	}
	return false, false, nil
}
