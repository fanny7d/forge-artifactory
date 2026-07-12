package blob

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"superfan.myasustor.com/fanchao/artifact-repository/internal/database"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/testenv"
)

func TestClaimReclaimsExpiredCreatingLeaseAndFencesOldOwner(t *testing.T) {
	pool := testenv.StartPostgres(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	now := &mutableClock{value: time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)}
	service, err := NewService(Options{Pool: pool, Clock: now, Lease: 2 * time.Minute})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	ownerOne := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	ownerTwo := uuid.MustParse("22222222-3333-4444-8555-666666666666")
	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	first, err := service.Claim(t.Context(), ClaimRequest{SHA256: sha, Size: 7, Owner: ownerOne})
	if err != nil {
		t.Fatalf("Claim(owner one) error = %v", err)
	}
	if first.Decision != DecisionOwned || first.Generation != 0 {
		t.Fatalf("first claim = %+v", first)
	}
	if _, err := service.Claim(t.Context(), ClaimRequest{SHA256: sha, Size: 7, Owner: ownerTwo}); !errors.Is(err, ErrInProgress) {
		t.Fatalf("Claim(active owner two) error = %v, want ErrInProgress", err)
	}

	now.value = now.value.Add(3 * time.Minute)
	reclaimed, err := service.Claim(t.Context(), ClaimRequest{SHA256: sha, Size: 7, Owner: ownerTwo})
	if err != nil {
		t.Fatalf("Claim(reclaimed) error = %v", err)
	}
	if reclaimed.Decision != DecisionOwned || reclaimed.Generation != 1 {
		t.Fatalf("reclaimed claim = %+v", reclaimed)
	}
	if _, err := service.MarkReady(t.Context(), Fence{SHA256: sha, Owner: ownerOne, Generation: 0}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("MarkReady(stale) error = %v, want ErrLeaseLost", err)
	}
	ready, err := service.MarkReady(t.Context(), Fence{SHA256: sha, Owner: ownerTwo, Generation: 1})
	if err != nil {
		t.Fatalf("MarkReady(owner two) error = %v", err)
	}
	if ready.State != StateReady {
		t.Fatalf("ready blob = %+v", ready)
	}
}

func TestReferenceRequiresReadyAndDeletingRejectsWriters(t *testing.T) {
	pool := testenv.StartPostgres(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	now := &mutableClock{value: time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)}
	service, err := NewService(Options{Pool: pool, Clock: now, Lease: 2 * time.Minute})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	owner := uuid.MustParse("33333333-4444-4555-8666-777777777777")
	sha := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	claim, err := service.Claim(t.Context(), ClaimRequest{SHA256: sha, Size: 9, Owner: owner})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}

	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin reference transaction: %v", err)
	}
	if _, err := service.Reference(t.Context(), tx, sha); !errors.Is(err, ErrNotReady) {
		_ = tx.Rollback(t.Context())
		t.Fatalf("Reference(creating) error = %v, want ErrNotReady", err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatalf("rollback reference transaction: %v", err)
	}
	if _, err := service.BeginDelete(t.Context(), DeleteRequest{SHA256: sha, Owner: owner}); !errors.Is(err, ErrInProgress) {
		t.Fatalf("BeginDelete(active creating) error = %v, want ErrInProgress", err)
	}

	if _, err := service.MarkReady(t.Context(), Fence{SHA256: sha, Owner: owner, Generation: claim.Generation}); err != nil {
		t.Fatalf("MarkReady() error = %v", err)
	}
	tx, err = pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin ready reference transaction: %v", err)
	}
	referenced, err := service.Reference(t.Context(), tx, sha)
	if err != nil {
		_ = tx.Rollback(t.Context())
		t.Fatalf("Reference(ready) error = %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("commit reference transaction: %v", err)
	}
	if referenced.State != StateReady {
		t.Fatalf("referenced blob = %+v", referenced)
	}

	if _, err := pool.Exec(t.Context(), "DELETE FROM artifacts WHERE blob_sha256 = $1", sha); err != nil {
		t.Fatalf("remove artifact references: %v", err)
	}
	deleting, err := service.BeginDelete(t.Context(), DeleteRequest{SHA256: sha, Owner: owner})
	if err != nil {
		t.Fatalf("BeginDelete(ready orphan) error = %v", err)
	}
	if deleting.State != StateDeleting {
		t.Fatalf("deleting blob = %+v", deleting)
	}
	if _, err := service.Claim(t.Context(), ClaimRequest{SHA256: sha, Size: 9, Owner: uuid.New()}); !errors.Is(err, ErrDeleting) {
		t.Fatalf("Claim(deleting) error = %v, want ErrDeleting", err)
	}
}

type mutableClock struct {
	value time.Time
}

func (c *mutableClock) Now() time.Time {
	return c.value
}
