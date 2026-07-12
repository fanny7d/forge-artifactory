package blob

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"superfan.myasustor.com/fanchao/artifact-repository/internal/clock"
	db "superfan.myasustor.com/fanchao/artifact-repository/internal/database/sqlc"
)

type State string

const (
	StateCreating State = "creating"
	StateReady    State = "ready"
	StateDeleting State = "deleting"
)

type Decision string

const (
	DecisionOwned Decision = "owned"
	DecisionReady Decision = "ready"
)

var (
	ErrNotFound     = errors.New("blob not found")
	ErrNotReady     = errors.New("blob is not ready")
	ErrInProgress   = errors.New("blob operation is in progress")
	ErrDeleting     = errors.New("blob is being deleted")
	ErrLeaseLost    = errors.New("blob lease lost")
	ErrSizeMismatch = errors.New("blob hash has a different size")
	ErrReferenced   = errors.New("blob still has references")
	ErrInvalid      = errors.New("invalid blob request")
)

var validSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

type Options struct {
	Pool  *pgxpool.Pool
	Clock clock.Clock
	Lease time.Duration
}

type Service struct {
	pool  *pgxpool.Pool
	clock clock.Clock
	lease time.Duration
}

type Blob struct {
	SHA256            string
	Size              int64
	ObjectKey         string
	State             State
	LeaseOwner        *uuid.UUID
	LeaseGeneration   int64
	LeaseExpiresAt    *time.Time
	DeleteCompletedAt *time.Time
	LastReferencedAt  *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type ClaimRequest struct {
	SHA256 string
	Size   int64
	Owner  uuid.UUID
}

type ClaimResult struct {
	Blob
	Decision   Decision
	Generation int64
}

type Fence struct {
	SHA256     string
	Owner      uuid.UUID
	Generation int64
}

type DeleteRequest struct {
	SHA256 string
	Owner  uuid.UUID
}

func NewService(options Options) (*Service, error) {
	if options.Pool == nil {
		return nil, fmt.Errorf("blob service: pool is nil")
	}
	if options.Clock == nil {
		return nil, fmt.Errorf("blob service: clock is nil")
	}
	if options.Lease <= 0 {
		return nil, fmt.Errorf("blob service: lease must be positive")
	}
	return &Service{pool: options.Pool, clock: options.Clock, lease: options.Lease}, nil
}

func (s *Service) Claim(ctx context.Context, request ClaimRequest) (ClaimResult, error) {
	if !validSHA256.MatchString(request.SHA256) || request.Size < 0 || request.Owner == uuid.Nil {
		return ClaimResult{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return ClaimResult{}, fmt.Errorf("begin blob claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	queries := db.New(tx)
	now := s.clock.Now().UTC()
	row, err := queries.GetBlobForUpdate(ctx, request.SHA256)
	if errors.Is(err, pgx.ErrNoRows) {
		owner := request.Owner
		row, err = queries.InsertCreatingBlob(ctx, db.InsertCreatingBlobParams{
			Sha256:         request.SHA256,
			Size:           request.Size,
			ObjectKey:      ObjectKey(request.SHA256),
			LeaseOwner:     &owner,
			LeaseExpiresAt: pgtype.Timestamptz{Time: now.Add(s.lease), Valid: true},
		})
		if err == nil {
			if err := tx.Commit(ctx); err != nil {
				return ClaimResult{}, fmt.Errorf("commit new blob claim: %w", err)
			}
			blob := blobFromRow(row)
			return ClaimResult{Blob: blob, Decision: DecisionOwned, Generation: blob.LeaseGeneration}, nil
		}
		if errors.Is(err, pgx.ErrNoRows) {
			row, err = queries.GetBlobForUpdate(ctx, request.SHA256)
		}
	}
	if err != nil {
		return ClaimResult{}, fmt.Errorf("lock blob claim: %w", err)
	}
	if row.Size != request.Size {
		return ClaimResult{}, ErrSizeMismatch
	}

	switch State(row.State) {
	case StateReady:
		if err := tx.Commit(ctx); err != nil {
			return ClaimResult{}, fmt.Errorf("commit ready blob claim: %w", err)
		}
		blob := blobFromRow(row)
		return ClaimResult{Blob: blob, Decision: DecisionReady, Generation: blob.LeaseGeneration}, nil
	case StateDeleting:
		return ClaimResult{}, ErrDeleting
	case StateCreating:
		if row.LeaseExpiresAt.Valid && row.LeaseExpiresAt.Time.After(now) {
			if row.LeaseOwner != nil && *row.LeaseOwner == request.Owner {
				if err := tx.Commit(ctx); err != nil {
					return ClaimResult{}, fmt.Errorf("commit existing blob claim: %w", err)
				}
				blob := blobFromRow(row)
				return ClaimResult{Blob: blob, Decision: DecisionOwned, Generation: blob.LeaseGeneration}, nil
			}
			return ClaimResult{}, ErrInProgress
		}
		owner := request.Owner
		row, err = queries.TakeExpiredCreatingBlob(ctx, db.TakeExpiredCreatingBlobParams{
			Sha256:         request.SHA256,
			LeaseOwner:     &owner,
			LeaseExpiresAt: pgtype.Timestamptz{Time: now.Add(s.lease), Valid: true},
			UpdatedAt:      pgtype.Timestamptz{Time: now, Valid: true},
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ClaimResult{}, ErrInProgress
		}
		if err != nil {
			return ClaimResult{}, fmt.Errorf("take expired blob claim: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return ClaimResult{}, fmt.Errorf("commit reclaimed blob: %w", err)
		}
		blob := blobFromRow(row)
		return ClaimResult{Blob: blob, Decision: DecisionOwned, Generation: blob.LeaseGeneration}, nil
	default:
		return ClaimResult{}, fmt.Errorf("unknown blob state %q", row.State)
	}
}

func (s *Service) MarkReady(ctx context.Context, fence Fence) (Blob, error) {
	if err := validateFence(fence); err != nil {
		return Blob{}, err
	}
	owner := fence.Owner
	row, err := db.New(s.pool).MarkBlobReady(ctx, db.MarkBlobReadyParams{
		Sha256:          fence.SHA256,
		LeaseOwner:      &owner,
		LeaseGeneration: fence.Generation,
		UpdatedAt:       pgtype.Timestamptz{Time: s.clock.Now().UTC(), Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Blob{}, ErrLeaseLost
	}
	if err != nil {
		return Blob{}, fmt.Errorf("mark blob ready: %w", err)
	}
	return blobFromRow(row), nil
}

func (s *Service) Reference(ctx context.Context, tx pgx.Tx, sha256 string) (Blob, error) {
	if tx == nil || !validSHA256.MatchString(sha256) {
		return Blob{}, ErrInvalid
	}
	queries := db.New(tx)
	row, err := queries.GetBlobForUpdate(ctx, sha256)
	if errors.Is(err, pgx.ErrNoRows) {
		return Blob{}, ErrNotFound
	}
	if err != nil {
		return Blob{}, fmt.Errorf("lock referenced blob: %w", err)
	}
	switch State(row.State) {
	case StateReady:
	case StateDeleting:
		return Blob{}, ErrDeleting
	default:
		return Blob{}, ErrNotReady
	}
	rows, err := queries.TouchReadyBlob(ctx, db.TouchReadyBlobParams{
		Sha256:           sha256,
		LastReferencedAt: pgtype.Timestamptz{Time: s.clock.Now().UTC(), Valid: true},
	})
	if err != nil {
		return Blob{}, fmt.Errorf("touch referenced blob: %w", err)
	}
	if rows != 1 {
		return Blob{}, ErrNotReady
	}
	return blobFromRow(row), nil
}

func (s *Service) BeginDelete(ctx context.Context, request DeleteRequest) (Blob, error) {
	if !validSHA256.MatchString(request.SHA256) || request.Owner == uuid.Nil {
		return Blob{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Blob{}, fmt.Errorf("begin blob delete: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := db.New(tx)
	row, err := queries.GetBlobForUpdate(ctx, request.SHA256)
	if errors.Is(err, pgx.ErrNoRows) {
		return Blob{}, ErrNotFound
	}
	if err != nil {
		return Blob{}, fmt.Errorf("lock blob for delete: %w", err)
	}
	now := s.clock.Now().UTC()
	switch State(row.State) {
	case StateDeleting:
		return Blob{}, ErrDeleting
	case StateCreating:
		if row.LeaseExpiresAt.Valid && row.LeaseExpiresAt.Time.After(now) {
			return Blob{}, ErrInProgress
		}
	case StateReady:
	default:
		return Blob{}, fmt.Errorf("unknown blob state %q", row.State)
	}
	owner := request.Owner
	row, err = queries.MarkOrphanBlobDeleting(ctx, db.MarkOrphanBlobDeletingParams{
		Sha256:         request.SHA256,
		LeaseOwner:     &owner,
		LeaseExpiresAt: pgtype.Timestamptz{Time: now.Add(s.lease), Valid: true},
		UpdatedAt:      pgtype.Timestamptz{Time: now, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Blob{}, ErrReferenced
	}
	if err != nil {
		return Blob{}, fmt.Errorf("mark blob deleting: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Blob{}, fmt.Errorf("commit blob delete claim: %w", err)
	}
	return blobFromRow(row), nil
}

func (s *Service) CompleteDelete(ctx context.Context, fence Fence, completedAt, quarantineUntil time.Time) error {
	if err := validateFence(fence); err != nil {
		return err
	}
	if completedAt.IsZero() || !quarantineUntil.After(completedAt) {
		return ErrInvalid
	}
	owner := fence.Owner
	rows, err := db.New(s.pool).MarkBlobDeleteCompleted(ctx, db.MarkBlobDeleteCompletedParams{
		Sha256:            fence.SHA256,
		LeaseOwner:        &owner,
		LeaseGeneration:   fence.Generation,
		DeleteCompletedAt: pgtype.Timestamptz{Time: completedAt.UTC(), Valid: true},
		LeaseExpiresAt:    pgtype.Timestamptz{Time: quarantineUntil.UTC(), Valid: true},
	})
	if err != nil {
		return fmt.Errorf("complete blob delete: %w", err)
	}
	if rows != 1 {
		return ErrLeaseLost
	}
	return nil
}

func ObjectKey(sha256 string) string {
	if len(sha256) < 4 {
		return ""
	}
	return "blobs/sha256/" + sha256[:2] + "/" + sha256[2:4] + "/" + sha256
}

func validateFence(fence Fence) error {
	if !validSHA256.MatchString(fence.SHA256) || fence.Owner == uuid.Nil || fence.Generation < 0 {
		return ErrInvalid
	}
	return nil
}

func blobFromRow(row db.Blob) Blob {
	return Blob{
		SHA256:            row.Sha256,
		Size:              row.Size,
		ObjectKey:         row.ObjectKey,
		State:             State(row.State),
		LeaseOwner:        row.LeaseOwner,
		LeaseGeneration:   row.LeaseGeneration,
		LeaseExpiresAt:    optionalTime(row.LeaseExpiresAt),
		DeleteCompletedAt: optionalTime(row.DeleteCompletedAt),
		LastReferencedAt:  optionalTime(row.LastReferencedAt),
		CreatedAt:         row.CreatedAt.Time.UTC(),
		UpdatedAt:         row.UpdatedAt.Time.UTC(),
	}
}

func optionalTime(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	timestamp := value.Time.UTC()
	return &timestamp
}
