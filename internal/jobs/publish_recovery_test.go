package jobs

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"superfan.myasustor.com/fanchao/artifact-repository/internal/clock"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/database"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/release"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/testenv"
)

func TestRecoverPublishingOnceSelectsOnlyDueActiveAttempts(t *testing.T) {
	pool, fixture := newPublishRecoveryFixture(t)
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	dueID := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	insertPublishRecoveryAttempt(t, pool, fixture, dueID, "1.0.0", "active", now.Add(-time.Minute), now.Add(-time.Second))
	insertPublishRecoveryAttempt(t, pool, fixture, uuid.MustParse("22222222-2222-4222-8222-222222222222"), "1.0.1", "active", now.Add(time.Minute), now.Add(-time.Second))
	insertPublishRecoveryAttempt(t, pool, fixture, uuid.MustParse("33333333-3333-4333-8333-333333333333"), "1.0.2", "active", now.Add(-time.Minute), now.Add(time.Minute))
	insertPublishRecoveryAttempt(t, pool, fixture, uuid.MustParse("44444444-4444-4444-8444-444444444444"), "1.0.3", "completed", now.Add(-time.Minute), now.Add(-time.Second))

	publisher := &publishRecovererStub{}
	recovery, err := NewPublishRecovery(PublishRecoveryOptions{
		Pool: pool, Publisher: publisher, Clock: clock.Fixed{Time: now}, BatchSize: 10,
	})
	if err != nil {
		t.Fatalf("NewPublishRecovery() error = %v", err)
	}
	if err := recovery.RecoverPublishingOnce(t.Context()); err != nil {
		t.Fatalf("RecoverPublishingOnce() error = %v", err)
	}
	if len(publisher.calls) != 1 || publisher.calls[0] != dueID {
		t.Fatalf("recovered attempts = %v, want [%s]", publisher.calls, dueID)
	}
}

func TestRecoverPublishingOnceContinuesAfterLeaseRace(t *testing.T) {
	pool, fixture := newPublishRecoveryFixture(t)
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	racedID := uuid.MustParse("55555555-5555-4555-8555-555555555555")
	recoveredID := uuid.MustParse("66666666-6666-4666-8666-666666666666")
	insertPublishRecoveryAttempt(t, pool, fixture, racedID, "2.0.0", "active", now.Add(-2*time.Minute), now.Add(-time.Second))
	insertPublishRecoveryAttempt(t, pool, fixture, recoveredID, "2.0.1", "active", now.Add(-time.Minute), now.Add(-time.Second))

	publisher := &publishRecovererStub{errors: map[uuid.UUID]error{racedID: release.ErrLeaseLost}}
	recovery, err := NewPublishRecovery(PublishRecoveryOptions{
		Pool: pool, Publisher: publisher, Clock: clock.Fixed{Time: now}, BatchSize: 10,
	})
	if err != nil {
		t.Fatalf("NewPublishRecovery() error = %v", err)
	}
	if err := recovery.RecoverPublishingOnce(t.Context()); err != nil {
		t.Fatalf("RecoverPublishingOnce() error = %v", err)
	}
	if len(publisher.calls) != 2 || publisher.calls[0] != racedID || publisher.calls[1] != recoveredID {
		t.Fatalf("recovered attempts = %v, want [%s %s]", publisher.calls, racedID, recoveredID)
	}
}

type publishRecovererStub struct {
	calls  []uuid.UUID
	errors map[uuid.UUID]error
}

func (s *publishRecovererStub) RecoverAttempt(_ context.Context, attemptID uuid.UUID) (release.PublishedRelease, error) {
	s.calls = append(s.calls, attemptID)
	return release.PublishedRelease{}, s.errors[attemptID]
}

type publishRecoveryFixture struct {
	tokenID   uuid.UUID
	packageID uuid.UUID
	keyID     string
}

func newPublishRecoveryFixture(t *testing.T) (*pgxpool.Pool, publishRecoveryFixture) {
	t.Helper()
	pool := testenv.StartPostgres(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	accountID := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	tokenID := uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	repositoryID := uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc")
	packageID := uuid.MustParse("dddddddd-dddd-4ddd-8ddd-dddddddddddd")
	keyID := "recovery-test-key"
	if _, err := pool.Exec(t.Context(), "INSERT INTO service_accounts (id, name) VALUES ($1, 'publish-recovery')", accountID); err != nil {
		t.Fatalf("insert service account: %v", err)
	}
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO api_tokens
		 (id, service_account_id, secret_hmac, scopes, repository_ids, expires_at)
		 VALUES ($1, $2, $3, ARRAY['admin'], '{}', $4)`,
		tokenID, accountID, bytes.Repeat([]byte{0x31}, 32), time.Now().UTC().Add(24*time.Hour),
	); err != nil {
		t.Fatalf("insert API token: %v", err)
	}
	if _, err := pool.Exec(t.Context(),
		"INSERT INTO repositories (id, key, display_name, created_by) VALUES ($1, 'recovery', 'Recovery', $2)",
		repositoryID, tokenID,
	); err != nil {
		t.Fatalf("insert repository: %v", err)
	}
	if _, err := pool.Exec(t.Context(),
		"INSERT INTO packages (id, repository_id, name, created_by) VALUES ($1, $2, 'edgecli', $3)",
		packageID, repositoryID, tokenID,
	); err != nil {
		t.Fatalf("insert package: %v", err)
	}
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO signing_keys (key_id, algorithm, public_key, fingerprint, active)
		 VALUES ($1, 'Ed25519', $2, $3, true)`,
		keyID, bytes.Repeat([]byte{0x41}, 32), string(bytes.Repeat([]byte{'a'}, 64)),
	); err != nil {
		t.Fatalf("insert signing key: %v", err)
	}
	return pool, publishRecoveryFixture{tokenID: tokenID, packageID: packageID, keyID: keyID}
}

func insertPublishRecoveryAttempt(
	t *testing.T,
	pool *pgxpool.Pool,
	fixture publishRecoveryFixture,
	attemptID uuid.UUID,
	version string,
	state string,
	leaseExpiresAt time.Time,
	nextRetryAt time.Time,
) {
	t.Helper()
	releaseID := uuid.New()
	if _, err := pool.Exec(t.Context(),
		"INSERT INTO releases (id, package_id, version, state, created_by) VALUES ($1, $2, $3, 'draft', $4)",
		releaseID, fixture.packageID, version, fixture.tokenID,
	); err != nil {
		t.Fatalf("insert release %s: %v", version, err)
	}
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO publish_attempts
		 (id, release_id, actor_token_id, request_id, published_at, snapshot, snapshot_sha256,
		  key_id, lease_owner, lease_expires_at, state, next_retry_at)
		 VALUES ($1, $2, $3, $4, $5, '{}'::jsonb, $6, $7, $8, $9, $10, $11)`,
		attemptID, releaseID, fixture.tokenID, "request-"+version, leaseExpiresAt,
		string(bytes.Repeat([]byte{'b'}, 64)), fixture.keyID, uuid.New(), leaseExpiresAt, state, nextRetryAt,
	); err != nil {
		t.Fatalf("insert publish attempt %s: %v", version, err)
	}
	if _, err := pool.Exec(t.Context(),
		"UPDATE releases SET state = 'publishing', current_attempt_id = $1 WHERE id = $2",
		attemptID, releaseID,
	); err != nil {
		t.Fatalf("link publish attempt %s: %v", version, err)
	}
}
