package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"superfan.myasustor.com/fanchao/artifact-repository/internal/clock"
	db "superfan.myasustor.com/fanchao/artifact-repository/internal/database/sqlc"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/id"
)

type Kind string

const (
	KindCleanupBlob        Kind = "cleanup_blob"
	KindCleanupUpload      Kind = "cleanup_upload"
	KindCleanupIdempotency Kind = "cleanup_idempotency"
	KindRecoverPublish     Kind = "recover_publish"
)

var ErrJobLeaseLost = errors.New("job lease lost")

type Handler func(context.Context) error

type Definition struct {
	Kind     Kind
	Interval time.Duration
	Handler  Handler
}

type RunnerOptions struct {
	Pool         *pgxpool.Pool
	Clock        clock.Clock
	IDs          id.Generator
	Definitions  []Definition
	Lease        time.Duration
	PollInterval time.Duration
	RetryBase    time.Duration
	RetryMax     time.Duration
	MaxAttempts  int32
}

type Runner struct {
	pool         *pgxpool.Pool
	clock        clock.Clock
	ids          id.Generator
	definitions  []Definition
	lease        time.Duration
	pollInterval time.Duration
	retryBase    time.Duration
	retryMax     time.Duration
	maxAttempts  int32
}

func NewRunner(options RunnerOptions) (*Runner, error) {
	if options.Pool == nil || options.Clock == nil || options.IDs == nil {
		return nil, fmt.Errorf("job runner requires database, clock, and ID dependencies")
	}
	if len(options.Definitions) == 0 {
		return nil, fmt.Errorf("job runner requires at least one definition")
	}
	if options.Lease <= 0 || options.PollInterval <= 0 || options.RetryBase <= 0 || options.RetryMax < options.RetryBase {
		return nil, fmt.Errorf("job runner durations are invalid")
	}
	if options.MaxAttempts < 1 || options.MaxAttempts > 100 {
		return nil, fmt.Errorf("job runner max attempts must be between 1 and 100")
	}
	seen := make(map[Kind]struct{}, len(options.Definitions))
	definitions := append([]Definition(nil), options.Definitions...)
	for _, definition := range definitions {
		if !validKind(definition.Kind) || definition.Interval <= 0 || definition.Handler == nil {
			return nil, fmt.Errorf("job runner definition for %q is invalid", definition.Kind)
		}
		if _, exists := seen[definition.Kind]; exists {
			return nil, fmt.Errorf("job runner definition for %q is duplicated", definition.Kind)
		}
		seen[definition.Kind] = struct{}{}
	}
	return &Runner{
		pool: options.Pool, clock: options.Clock, ids: options.IDs, definitions: definitions,
		lease: options.Lease, pollInterval: options.PollInterval, retryBase: options.RetryBase,
		retryMax: options.RetryMax, maxAttempts: options.MaxAttempts,
	}, nil
}

func (r *Runner) Run(ctx context.Context) error {
	if ctx.Err() != nil {
		return nil
	}
	if err := r.enqueueScheduledJobs(ctx); err != nil {
		return err
	}
	for {
		if ctx.Err() != nil {
			return nil
		}
		processed, err := r.runOnce(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if processed {
			continue
		}
		timer := time.NewTimer(r.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func (r *Runner) enqueueScheduledJobs(ctx context.Context) error {
	for _, definition := range r.definitions {
		if err := r.ensureScheduled(ctx, definition); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) runOnce(ctx context.Context) (bool, error) {
	processed := false
	for _, definition := range r.definitions {
		if err := r.ensureScheduled(ctx, definition); err != nil {
			return processed, err
		}
		now := r.clock.Now().UTC()
		owner := r.ids.New()
		if owner == uuid.Nil {
			return processed, fmt.Errorf("generate job lease owner: nil UUID")
		}
		job, err := db.New(r.pool).ClaimJob(ctx, db.ClaimJobParams{
			Kind:           string(definition.Kind),
			LeaseOwner:     &owner,
			LeaseExpiresAt: pgtype.Timestamptz{Time: now.Add(r.lease), Valid: true},
			UpdatedAt:      pgtype.Timestamptz{Time: now, Valid: true},
		})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return processed, fmt.Errorf("claim %s job: %w", definition.Kind, err)
		}
		processed = true
		handlerErr := definition.Handler(ctx)
		if ctx.Err() != nil {
			return processed, ctx.Err()
		}
		if handlerErr != nil {
			if err := r.retry(ctx, job, owner, definition); err != nil {
				if errors.Is(err, ErrJobLeaseLost) {
					continue
				}
				return processed, err
			}
			continue
		}
		if err := r.completeAndSchedule(ctx, job, owner, definition); err != nil {
			if errors.Is(err, ErrJobLeaseLost) {
				continue
			}
			return processed, err
		}
	}
	return processed, nil
}

func (r *Runner) ensureScheduled(ctx context.Context, definition Definition) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin %s job scheduling: %w", definition.Kind, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	now := r.clock.Now().UTC()
	rows, err := db.New(tx).ReapExpiredExhaustedJob(ctx, db.ReapExpiredExhaustedJobParams{
		Kind:      string(definition.Kind),
		UpdatedAt: pgtype.Timestamptz{Time: now, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("reap exhausted %s job: %w", definition.Kind, err)
	}
	availableAt := now
	if rows != 0 {
		availableAt = now.Add(definition.Interval)
	}
	if err := r.enqueue(ctx, db.New(tx), definition, availableAt); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit %s job scheduling: %w", definition.Kind, err)
	}
	return nil
}

func (r *Runner) retry(ctx context.Context, job db.Job, owner uuid.UUID, definition Definition) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin %s job retry: %w", definition.Kind, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	now := r.clock.Now().UTC()
	code := strings.ReplaceAll(string(definition.Kind), "_", "-") + "-failed"
	rows, err := db.New(tx).RetryJob(ctx, db.RetryJobParams{
		ID: job.ID, LeaseOwner: &owner, LeaseGeneration: job.LeaseGeneration, FailureCode: &code,
		AvailableAt: pgtype.Timestamptz{Time: now.Add(r.retryBackoff(job.Attempts)), Valid: true},
		UpdatedAt:   pgtype.Timestamptz{Time: now, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("retry %s job: %w", definition.Kind, err)
	}
	if rows != 1 {
		return ErrJobLeaseLost
	}
	if job.Attempts >= job.MaxAttempts {
		if err := r.enqueue(ctx, db.New(tx), definition, now.Add(definition.Interval)); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit %s job retry: %w", definition.Kind, err)
	}
	return nil
}

func (r *Runner) completeAndSchedule(ctx context.Context, job db.Job, owner uuid.UUID, definition Definition) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin %s job completion: %w", definition.Kind, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	now := r.clock.Now().UTC()
	rows, err := db.New(tx).CompleteJob(ctx, db.CompleteJobParams{
		ID: job.ID, LeaseOwner: &owner, LeaseGeneration: job.LeaseGeneration,
		UpdatedAt: pgtype.Timestamptz{Time: now, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("complete %s job: %w", definition.Kind, err)
	}
	if rows != 1 {
		return ErrJobLeaseLost
	}
	if err := r.enqueue(ctx, db.New(tx), definition, now.Add(definition.Interval)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit %s job completion: %w", definition.Kind, err)
	}
	return nil
}

type jobQueries interface {
	EnqueueJob(context.Context, db.EnqueueJobParams) (db.Job, error)
}

func (r *Runner) enqueue(ctx context.Context, queries jobQueries, definition Definition, availableAt time.Time) error {
	_, err := queries.EnqueueJob(ctx, db.EnqueueJobParams{
		Kind: string(definition.Kind), Payload: []byte("{}"), MaxAttempts: r.maxAttempts,
		AvailableAt: pgtype.Timestamptz{Time: availableAt.UTC(), Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("enqueue %s job: %w", definition.Kind, err)
	}
	return nil
}

func (r *Runner) retryBackoff(attempt int32) time.Duration {
	delay := r.retryBase
	for current := int32(1); current < attempt && delay < r.retryMax; current++ {
		if delay > r.retryMax/2 {
			return r.retryMax
		}
		delay *= 2
	}
	if delay > r.retryMax {
		return r.retryMax
	}
	return delay
}

func validKind(kind Kind) bool {
	switch kind {
	case KindCleanupBlob, KindCleanupUpload, KindCleanupIdempotency, KindRecoverPublish:
		return true
	default:
		return false
	}
}
