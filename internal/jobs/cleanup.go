package jobs

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"superfan.myasustor.com/fanchao/artifact-repository/internal/blob"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/clock"
	db "superfan.myasustor.com/fanchao/artifact-repository/internal/database/sqlc"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/id"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/storage"
)

type CleanupOptions struct {
	Pool             *pgxpool.Pool
	Blobs            *blob.Service
	Store            storage.Store
	Clock            clock.Clock
	IDs              id.Generator
	OrphanRetention  time.Duration
	DeleteLease      time.Duration
	DeleteHeartbeat  time.Duration
	DeleteTimeout    time.Duration
	DeleteQuarantine time.Duration
	BatchSize        int32
}

type Cleaner struct {
	pool             *pgxpool.Pool
	blobs            *blob.Service
	store            storage.Store
	clock            clock.Clock
	ids              id.Generator
	orphanRetention  time.Duration
	deleteLease      time.Duration
	deleteHeartbeat  time.Duration
	deleteTimeout    time.Duration
	deleteQuarantine time.Duration
	batchSize        int32
	listMu           sync.Mutex
	blobListAfter    string
	stagingListAfter string
}

func NewCleaner(options CleanupOptions) (*Cleaner, error) {
	if options.Pool == nil || options.Blobs == nil || options.Store == nil || options.Clock == nil || options.IDs == nil {
		return nil, fmt.Errorf("cleaner requires database, blob, storage, clock, and ID dependencies")
	}
	heartbeat := options.DeleteHeartbeat
	if heartbeat == 0 {
		heartbeat = options.DeleteLease / 3
	}
	timeout := options.DeleteTimeout
	if timeout == 0 {
		timeout = options.DeleteLease * 2
	}
	quarantine := options.DeleteQuarantine
	if quarantine == 0 {
		quarantine = options.DeleteLease
	}
	if options.OrphanRetention <= 0 || options.DeleteLease <= 0 || heartbeat <= 0 || heartbeat >= options.DeleteLease || timeout <= 0 || quarantine <= 0 || options.BatchSize <= 0 || options.BatchSize > 1000 {
		return nil, fmt.Errorf("cleaner retention and lease must be positive, and batch size must be between 1 and 1000")
	}
	return &Cleaner{
		pool:             options.Pool,
		blobs:            options.Blobs,
		store:            options.Store,
		clock:            options.Clock,
		ids:              options.IDs,
		orphanRetention:  options.OrphanRetention,
		deleteLease:      options.DeleteLease,
		deleteHeartbeat:  heartbeat,
		deleteTimeout:    timeout,
		deleteQuarantine: quarantine,
		batchSize:        options.BatchSize,
	}, nil
}

func (c *Cleaner) CleanupBlobsOnce(ctx context.Context) error {
	var cleanupErrors []error
	for count := int32(0); count < c.batchSize; count++ {
		now := c.clock.Now().UTC()
		if _, err := db.New(c.pool).DeleteQuarantinedBlob(ctx, pgtype.Timestamptz{Time: now, Valid: true}); err == nil {
			continue
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return errors.Join(append(cleanupErrors, fmt.Errorf("delete quarantined blob tombstone: %w", err))...)
		}
		owner := c.ids.New()
		if owner == uuid.Nil {
			return fmt.Errorf("generate blob cleanup owner: nil UUID")
		}
		row, err := db.New(c.pool).ClaimOrphanBlob(ctx, db.ClaimOrphanBlobParams{
			LeaseOwner:     &owner,
			LeaseExpiresAt: pgtype.Timestamptz{Time: now.Add(c.deleteLease), Valid: true},
			Now:            pgtype.Timestamptz{Time: now, Valid: true},
			Cutoff:         pgtype.Timestamptz{Time: now.Add(-c.orphanRetention), Valid: true},
		})
		if errors.Is(err, pgx.ErrNoRows) {
			cleanupErrors = append(cleanupErrors, c.reconcileStorageBlobObjects(ctx, int(c.batchSize-count)))
			return errors.Join(cleanupErrors...)
		}
		if err != nil {
			return fmt.Errorf("claim orphan blob: %w", err)
		}
		if err := c.deleteBlobObject(ctx, row, owner); err != nil && !errors.Is(err, storage.ErrNotFound) {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("delete orphan blob object %q: %w", row.ObjectKey, err))
			continue
		}
		completedAt := c.clock.Now().UTC()
		if err := c.blobs.CompleteDelete(
			ctx,
			blob.Fence{SHA256: row.Sha256, Owner: owner, Generation: row.LeaseGeneration},
			completedAt,
			completedAt.Add(c.deleteQuarantine),
		); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("complete orphan blob deletion %q: %w", row.Sha256, err))
		}
	}
	return errors.Join(cleanupErrors...)
}

func (c *Cleaner) reconcileStorageBlobObjects(ctx context.Context, limit int) error {
	if limit <= 0 {
		return nil
	}
	c.listMu.Lock()
	defer c.listMu.Unlock()

	page, err := c.store.List(ctx, storage.ListRequest{
		Prefix: "blobs/sha256/",
		After:  c.blobListAfter,
		Limit:  limit,
	})
	if err != nil {
		return fmt.Errorf("list storage Blob objects: %w", err)
	}
	c.blobListAfter = page.NextAfter

	now := c.clock.Now().UTC()
	cutoff := now.Add(-c.orphanRetention)
	queries := db.New(c.pool)
	var cleanupErrors []error
	for _, object := range page.Items {
		sha256, ok := storageBlobSHA(object.Key)
		if !ok || object.Size < 0 || object.LastModified.IsZero() || !object.LastModified.Before(cutoff) {
			continue
		}
		owner := c.ids.New()
		if owner == uuid.Nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("generate storage Blob cleanup owner: nil UUID"))
			continue
		}
		row, err := queries.InsertStorageOrphanBlobTombstone(ctx, db.InsertStorageOrphanBlobTombstoneParams{
			Sha256:         sha256,
			Size:           object.Size,
			ObjectKey:      object.Key,
			LeaseOwner:     &owner,
			LeaseExpiresAt: pgtype.Timestamptz{Time: now.Add(c.deleteLease), Valid: true},
			Now:            pgtype.Timestamptz{Time: now, Valid: true},
		})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("insert storage Blob tombstone %q: %w", object.Key, err))
			continue
		}
		if err := c.deleteBlobObject(ctx, row, owner); err != nil && !errors.Is(err, storage.ErrNotFound) {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("delete storage Blob object %q: %w", object.Key, err))
			continue
		}
		completedAt := c.clock.Now().UTC()
		if err := c.blobs.CompleteDelete(
			ctx,
			blob.Fence{SHA256: row.Sha256, Owner: owner, Generation: row.LeaseGeneration},
			completedAt,
			completedAt.Add(c.deleteQuarantine),
		); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("complete storage Blob deletion %q: %w", row.Sha256, err))
		}
	}
	return errors.Join(cleanupErrors...)
}

func storageBlobSHA(key string) (string, bool) {
	const prefix = "blobs/sha256/"
	if !strings.HasPrefix(key, prefix) {
		return "", false
	}
	parts := strings.Split(key[len(prefix):], "/")
	if len(parts) != 3 || len(parts[2]) != 64 || parts[2] != strings.ToLower(parts[2]) {
		return "", false
	}
	if _, err := hex.DecodeString(parts[2]); err != nil || blob.ObjectKey(parts[2]) != key {
		return "", false
	}
	return parts[2], true
}

func (c *Cleaner) deleteBlobObject(ctx context.Context, row db.Blob, owner uuid.UUID) error {
	operationContext, cancel := context.WithTimeout(ctx, c.deleteTimeout)
	defer cancel()
	heartbeatResult := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(c.deleteHeartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-operationContext.Done():
				heartbeatResult <- nil
				return
			case <-ticker.C:
				now := c.clock.Now().UTC()
				rows, err := db.New(c.pool).RenewBlobDeleteLease(operationContext, db.RenewBlobDeleteLeaseParams{
					Sha256: row.Sha256, LeaseOwner: &owner, LeaseGeneration: row.LeaseGeneration,
					LeaseExpiresAt: pgtype.Timestamptz{Time: now.Add(c.deleteLease), Valid: true},
					UpdatedAt:      pgtype.Timestamptz{Time: now, Valid: true},
				})
				if err != nil {
					if err := heartbeatError(operationContext, err); err != nil {
						heartbeatResult <- fmt.Errorf("renew blob delete lease: %w", err)
					} else {
						heartbeatResult <- nil
					}
					cancel()
					return
				}
				if operationContext.Err() != nil {
					heartbeatResult <- nil
					return
				}
				if rows != 1 {
					heartbeatResult <- blob.ErrLeaseLost
					cancel()
					return
				}
			}
		}
	}()
	deleteErr := c.store.Delete(operationContext, row.ObjectKey)
	cancel()
	if heartbeatErr := <-heartbeatResult; heartbeatErr != nil {
		return heartbeatErr
	}
	return deleteErr
}

func heartbeatError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return nil
	}
	return err
}
