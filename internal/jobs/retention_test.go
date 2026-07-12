package jobs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"superfan.myasustor.com/fanchao/artifact-repository/internal/blob"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/clock"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/database"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/id"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/storage"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/testenv"
)

func TestCleanupUploadsRequiresExpiredLeaseAndHardDeadline(t *testing.T) {
	pool, tokenID, repositoryID := newRetentionFixture(t)
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	sessions := []struct {
		id           uuid.UUID
		key          string
		leaseExpires time.Time
		hardDeadline time.Time
	}{
		{uuid.MustParse("11111111-aaaa-4bbb-8ccc-222222222222"), "staging/eligible", now.Add(-time.Hour), now.Add(-time.Minute)},
		{uuid.MustParse("22222222-bbbb-4ccc-8ddd-333333333333"), "staging/active-lease", now.Add(time.Hour), now.Add(-time.Minute)},
		{uuid.MustParse("33333333-cccc-4ddd-8eee-444444444444"), "staging/live-deadline", now.Add(-time.Hour), now.Add(time.Hour)},
	}
	for _, session := range sessions {
		if _, err := pool.Exec(t.Context(),
			`INSERT INTO upload_sessions
			 (id, repository_id, logical_path, staging_key, state, lease_owner, lease_generation,
			  lease_expires_at, hard_deadline, last_heartbeat_at, created_by, created_at)
			 VALUES ($1, $2, $3, $4, 'active', $5, 0, $6, $7, $8, $9, $10)`,
			session.id, repositoryID, session.key, session.key, uuid.New(), session.leaseExpires,
			session.hardDeadline, now.Add(-2*time.Hour), tokenID, now.Add(-3*time.Hour),
		); err != nil {
			t.Fatalf("insert upload session %s: %v", session.key, err)
		}
	}
	store := &jobMemoryStore{objects: map[string][]byte{
		"staging/eligible":      []byte("eligible"),
		"staging/active-lease":  []byte("active"),
		"staging/live-deadline": []byte("deadline"),
	}}
	cleaner := newRetentionCleaner(t, pool, now, store)
	if err := cleaner.CleanupUploadsOnce(t.Context()); err != nil {
		t.Fatalf("CleanupUploadsOnce() error = %v", err)
	}
	for _, session := range sessions {
		var state string
		if err := pool.QueryRow(t.Context(), "SELECT state FROM upload_sessions WHERE id = $1", session.id).Scan(&state); err != nil {
			t.Fatalf("load upload session %s: %v", session.key, err)
		}
		_, objectExists := store.objects[session.key]
		if session.key == "staging/eligible" {
			if state != "failed" || objectExists {
				t.Fatalf("eligible session = state %q objectExists %t", state, objectExists)
			}
		} else if state != "active" || !objectExists {
			t.Fatalf("retained session %s = state %q objectExists %t", session.key, state, objectExists)
		}
	}
}

func TestCleanupUploadsRetainsLeaseAfterObjectDeleteFailure(t *testing.T) {
	pool, tokenID, repositoryID := newRetentionFixture(t)
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	sessionID := uuid.MustParse("77777777-7777-4777-8777-777777777777")
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO upload_sessions
		 (id, repository_id, logical_path, staging_key, state, lease_owner, lease_generation,
		  lease_expires_at, hard_deadline, last_heartbeat_at, created_by, created_at)
		 VALUES ($1, $2, 'retryable', 'staging/retryable', 'active', $3, 0, $4, $5, $6, $7, $8)`,
		sessionID, repositoryID, uuid.New(), now.Add(-time.Hour), now.Add(-time.Minute),
		now.Add(-2*time.Hour), tokenID, now.Add(-3*time.Hour),
	); err != nil {
		t.Fatalf("insert retryable upload session: %v", err)
	}
	store := &jobMemoryStore{
		objects:        map[string][]byte{"staging/retryable": []byte("payload")},
		deleteFailures: 1,
	}
	cleanupClock := &runnerMutableClock{now: now}
	cleaner := newRetentionCleanerWithClock(t, pool, cleanupClock, store)
	if err := cleaner.CleanupUploadsOnce(t.Context()); err == nil {
		t.Fatal("CleanupUploadsOnce(first) error = nil, want injected deletion failure")
	}
	var state string
	var generation int64
	var leaseExpiresAt time.Time
	if err := pool.QueryRow(t.Context(),
		"SELECT state, lease_generation, lease_expires_at FROM upload_sessions WHERE id = $1", sessionID,
	).Scan(&state, &generation, &leaseExpiresAt); err != nil {
		t.Fatalf("load failed cleanup lease: %v", err)
	}
	if state != "active" || generation != 1 || !leaseExpiresAt.After(now) {
		t.Fatalf("failed cleanup lease = state %q generation %d expiry %s", state, generation, leaseExpiresAt)
	}
	if err := cleaner.CleanupUploadsOnce(t.Context()); err != nil {
		t.Fatalf("CleanupUploadsOnce(before lease expiry) error = %v", err)
	}
	if _, exists := store.objects["staging/retryable"]; !exists {
		t.Fatal("retryable staging object deleted before lease expiry")
	}
	cleanupClock.Advance(2 * time.Minute)
	if err := cleaner.CleanupUploadsOnce(t.Context()); err != nil {
		t.Fatalf("CleanupUploadsOnce(retry) error = %v", err)
	}
	if err := pool.QueryRow(t.Context(), "SELECT state, lease_generation FROM upload_sessions WHERE id = $1", sessionID).Scan(&state, &generation); err != nil {
		t.Fatalf("load retried upload cleanup: %v", err)
	}
	if state != "failed" || generation != 2 {
		t.Fatalf("retried upload cleanup = state %q generation %d", state, generation)
	}
	if _, exists := store.objects["staging/retryable"]; exists {
		t.Fatal("retryable staging object still exists")
	}
}

func TestCleanupUploadsDeletesFailedSessionStaging(t *testing.T) {
	pool, tokenID, repositoryID := newRetentionFixture(t)
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	sessionID := uuid.MustParse("aaaaaaaa-7777-4777-8777-aaaaaaaaaaaa")
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO upload_sessions
		 (id, repository_id, logical_path, staging_key, state, lease_owner, lease_generation,
		  lease_expires_at, hard_deadline, last_heartbeat_at, created_by, created_at)
		 VALUES ($1, $2, 'failed', 'staging/failed', 'failed', $3, 0, $4, $5, $6, $7, $8)`,
		sessionID, repositoryID, uuid.New(), now.Add(-time.Hour), now.Add(-time.Minute),
		now.Add(-2*time.Hour), tokenID, now.Add(-3*time.Hour),
	); err != nil {
		t.Fatalf("insert failed upload session: %v", err)
	}
	store := &jobMemoryStore{objects: map[string][]byte{"staging/failed": []byte("payload")}}
	cleaner := newRetentionCleaner(t, pool, now, store)
	if err := cleaner.CleanupUploadsOnce(t.Context()); err != nil {
		t.Fatalf("CleanupUploadsOnce() error = %v", err)
	}
	var state string
	var cleanupCompletedAt *time.Time
	if err := pool.QueryRow(t.Context(),
		"SELECT state, cleanup_completed_at FROM upload_sessions WHERE id = $1", sessionID,
	).Scan(&state, &cleanupCompletedAt); err != nil {
		t.Fatalf("load failed session cleanup: %v", err)
	}
	if state != "failed" || cleanupCompletedAt == nil {
		t.Fatalf("failed session cleanup = state %q completedAt %v", state, cleanupCompletedAt)
	}
	if _, exists := store.objects["staging/failed"]; exists {
		t.Fatal("failed session staging object still exists")
	}
}

func TestCleanupUploadsDeletesRowlessStagingObjects(t *testing.T) {
	pool, _, _ := newRetentionFixture(t)
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	uploadKey := "staging/uploads/11111111-1111-4111-8111-111111111111"
	publishKey := "staging/publish/22222222-2222-4222-8222-222222222222/manifest"
	store := &jobMemoryStore{
		objects: map[string][]byte{
			uploadKey:  []byte("upload"),
			publishKey: []byte("manifest"),
		},
		modified: map[string]time.Time{
			uploadKey:  now.Add(-48 * time.Hour),
			publishKey: now.Add(-48 * time.Hour),
		},
	}
	cleaner := newRetentionCleaner(t, pool, now, store)
	if err := cleaner.CleanupUploadsOnce(t.Context()); err != nil {
		t.Fatalf("CleanupUploadsOnce() error = %v", err)
	}
	if _, exists := store.objects[uploadKey]; exists {
		t.Fatalf("rowless upload staging object %q still exists", uploadKey)
	}
	if _, exists := store.objects[publishKey]; exists {
		t.Fatalf("rowless publish staging object %q still exists", publishKey)
	}
}

func TestCleanupUploadsRetainsStagingOwnedByActiveSession(t *testing.T) {
	pool, tokenID, repositoryID := newRetentionFixture(t)
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	sessionID := uuid.MustParse("33333333-3333-4333-8333-333333333333")
	stagingKey := "staging/uploads/" + sessionID.String()
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO upload_sessions
		 (id, repository_id, logical_path, staging_key, state, lease_owner, lease_expires_at,
		  hard_deadline, last_heartbeat_at, created_by, created_at)
		 VALUES ($1, $2, 'owned', $3, 'active', $1, $4, $4, $5, $6, $7)`,
		sessionID, repositoryID, stagingKey, now.Add(time.Hour), now, tokenID, now.Add(-time.Hour),
	); err != nil {
		t.Fatalf("insert active upload session: %v", err)
	}
	store := &jobMemoryStore{
		objects:  map[string][]byte{stagingKey: []byte("active")},
		modified: map[string]time.Time{stagingKey: now.Add(-48 * time.Hour)},
	}
	cleaner := newRetentionCleaner(t, pool, now, store)
	if err := cleaner.CleanupUploadsOnce(t.Context()); err != nil {
		t.Fatalf("CleanupUploadsOnce() error = %v", err)
	}
	if _, exists := store.objects[stagingKey]; !exists {
		t.Fatal("staging object owned by active UploadSession was deleted")
	}
}

func TestCleanupUploadsRetainsStagingOwnedByActivePublishAttempt(t *testing.T) {
	pool, fixture := newPublishRecoveryFixture(t)
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	attemptID := uuid.MustParse("44444444-4444-4444-8444-444444444444")
	insertPublishRecoveryAttempt(t, pool, fixture, attemptID, "3.0.0", "active", now.Add(time.Hour), now.Add(time.Hour))
	stagingKey := "staging/publish/" + attemptID.String() + "/manifest"
	store := &jobMemoryStore{
		objects:  map[string][]byte{stagingKey: []byte("active")},
		modified: map[string]time.Time{stagingKey: now.Add(-48 * time.Hour)},
	}
	cleaner := newRetentionCleaner(t, pool, now, store)
	if err := cleaner.CleanupUploadsOnce(t.Context()); err != nil {
		t.Fatalf("CleanupUploadsOnce() error = %v", err)
	}
	if _, exists := store.objects[stagingKey]; !exists {
		t.Fatal("staging object owned by active PublishAttempt was deleted")
	}
}

func TestCleanupUploadsAdvancesBoundedStagingListing(t *testing.T) {
	pool, tokenID, repositoryID := newRetentionFixture(t)
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	sessionID := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	ownedKey := "staging/uploads/" + sessionID.String()
	orphanKey := "staging/uploads/22222222-2222-4222-8222-222222222222"
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO upload_sessions
		 (id, repository_id, logical_path, staging_key, state, lease_owner, lease_expires_at,
		  hard_deadline, last_heartbeat_at, created_by, created_at)
		 VALUES ($1, $2, 'owned-page', $3, 'active', $1, $4, $4, $5, $6, $7)`,
		sessionID, repositoryID, ownedKey, now.Add(time.Hour), now, tokenID, now.Add(-time.Hour),
	); err != nil {
		t.Fatalf("insert active upload session: %v", err)
	}
	store := &jobMemoryStore{
		objects: map[string][]byte{ownedKey: []byte("active"), orphanKey: []byte("orphan")},
		modified: map[string]time.Time{
			ownedKey:  now.Add(-48 * time.Hour),
			orphanKey: now.Add(-48 * time.Hour),
		},
	}
	blobs, err := blob.NewService(blob.Options{Pool: pool, Clock: clock.Fixed{Time: now}, Lease: time.Minute})
	if err != nil {
		t.Fatalf("NewService(blob) error = %v", err)
	}
	cleaner, err := NewCleaner(CleanupOptions{
		Pool: pool, Blobs: blobs, Store: store, Clock: clock.Fixed{Time: now}, IDs: id.UUIDGenerator{},
		OrphanRetention: 24 * time.Hour, DeleteLease: time.Minute, BatchSize: 1,
	})
	if err != nil {
		t.Fatalf("NewCleaner() error = %v", err)
	}
	if err := cleaner.CleanupUploadsOnce(t.Context()); err != nil {
		t.Fatalf("CleanupUploadsOnce(first page) error = %v", err)
	}
	if _, exists := store.objects[orphanKey]; !exists {
		t.Fatal("orphan from second staging page was deleted during first bounded listing")
	}
	if err := cleaner.CleanupUploadsOnce(t.Context()); err != nil {
		t.Fatalf("CleanupUploadsOnce(second page) error = %v", err)
	}
	if _, exists := store.objects[ownedKey]; !exists {
		t.Fatal("owned staging object was deleted during reconciliation")
	}
	if _, exists := store.objects[orphanKey]; exists {
		t.Fatal("rowless staging object from second page still exists")
	}
}

func TestCleanupUploadsContinuesAfterOneObjectDeleteFailure(t *testing.T) {
	pool, tokenID, repositoryID := newRetentionFixture(t)
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	firstID := uuid.MustParse("bbbbbbbb-7777-4777-8777-bbbbbbbbbbbb")
	secondID := uuid.MustParse("cccccccc-7777-4777-8777-cccccccccccc")
	for index, session := range []struct {
		id  uuid.UUID
		key string
	}{{firstID, "staging/first"}, {secondID, "staging/second"}} {
		if _, err := pool.Exec(t.Context(),
			`INSERT INTO upload_sessions
			 (id, repository_id, logical_path, staging_key, state, lease_owner, lease_generation,
			  lease_expires_at, hard_deadline, last_heartbeat_at, created_by, created_at)
			 VALUES ($1, $2, $3, $3, 'active', $4, 0, $5, $6, $7, $8, $9)`,
			session.id, repositoryID, session.key, uuid.New(), now.Add(-time.Hour),
			now.Add(time.Duration(index-2)*time.Minute), now.Add(-2*time.Hour), tokenID, now.Add(-3*time.Hour),
		); err != nil {
			t.Fatalf("insert upload session %s: %v", session.key, err)
		}
	}
	store := &jobMemoryStore{
		objects: map[string][]byte{
			"staging/first":  []byte("first"),
			"staging/second": []byte("second"),
		},
		deleteFailures: 1,
	}
	cleaner := newRetentionCleaner(t, pool, now, store)
	if err := cleaner.CleanupUploadsOnce(t.Context()); err == nil {
		t.Fatal("CleanupUploadsOnce() error = nil, want first deletion failure")
	}
	var firstState, secondState string
	if err := pool.QueryRow(t.Context(), "SELECT state FROM upload_sessions WHERE id = $1", firstID).Scan(&firstState); err != nil {
		t.Fatalf("load first session: %v", err)
	}
	if err := pool.QueryRow(t.Context(), "SELECT state FROM upload_sessions WHERE id = $1", secondID).Scan(&secondState); err != nil {
		t.Fatalf("load second session: %v", err)
	}
	_, firstExists := store.objects["staging/first"]
	_, secondExists := store.objects["staging/second"]
	if firstState != "active" || !firstExists || secondState != "failed" || secondExists {
		t.Fatalf("cleanup fairness = first(%q,%t) second(%q,%t)", firstState, firstExists, secondState, secondExists)
	}
}

func TestCleanupIdempotencyDeletesOnlyExpiredCompletedRecords(t *testing.T) {
	pool, tokenID, _ := newRetentionFixture(t)
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	insertRecord := func(key, state string, expires time.Time) {
		t.Helper()
		if state == "completed" {
			if _, err := pool.Exec(t.Context(),
				`INSERT INTO idempotency_records
				 (token_id, http_method, canonical_resource, idempotency_key, request_fingerprint,
				  state, http_status, response_body, expires_at, completed_at)
				 VALUES ($1, 'POST', '/resource', $2, $3, 'completed', 200, '{}', $4, $5)`,
				tokenID, key, bytes.Repeat([]byte{0x21}, 32), expires, now.Add(-time.Hour),
			); err != nil {
				t.Fatalf("insert completed record %s: %v", key, err)
			}
			return
		}
		if _, err := pool.Exec(t.Context(),
			`INSERT INTO idempotency_records
			 (token_id, http_method, canonical_resource, idempotency_key, request_fingerprint, state, expires_at)
			 VALUES ($1, 'POST', '/resource', $2, $3, 'pending', $4)`,
			tokenID, key, bytes.Repeat([]byte{0x22}, 32), expires,
		); err != nil {
			t.Fatalf("insert pending record %s: %v", key, err)
		}
	}
	insertRecord("expired-completed", "completed", now.Add(-time.Minute))
	insertRecord("live-completed", "completed", now.Add(time.Hour))
	insertRecord("expired-pending", "pending", now.Add(-time.Minute))
	cleaner := newRetentionCleaner(t, pool, now, &jobMemoryStore{objects: make(map[string][]byte)})
	if err := cleaner.CleanupIdempotencyOnce(t.Context()); err != nil {
		t.Fatalf("CleanupIdempotencyOnce() error = %v", err)
	}
	rows, err := pool.Query(t.Context(), "SELECT idempotency_key FROM idempotency_records ORDER BY idempotency_key")
	if err != nil {
		t.Fatalf("list retained idempotency records: %v", err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatalf("scan retained key: %v", err)
		}
		keys = append(keys, key)
	}
	if len(keys) != 2 || keys[0] != "expired-pending" || keys[1] != "live-completed" {
		t.Fatalf("retained idempotency keys = %v", keys)
	}
}

func TestCleanupIdempotencyClearsTerminalPublishAttemptReferences(t *testing.T) {
	pool, fixture := newPublishRecoveryFixture(t)
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	referencedID := uuid.MustParse("88888888-8888-4888-8888-888888888888")
	standaloneID := uuid.MustParse("99999999-9999-4999-8999-999999999999")
	for _, record := range []struct {
		id        uuid.UUID
		key       string
		encrypted bool
	}{
		{referencedID, "referenced-publish", false},
		{standaloneID, "encrypted-token-response", true},
	} {
		if _, err := pool.Exec(t.Context(),
			`INSERT INTO idempotency_records
			 (id, token_id, http_method, canonical_resource, idempotency_key, request_fingerprint,
			  state, http_status, response_body, response_encrypted, expires_at, completed_at)
			 VALUES ($1, $2, 'POST', '/resource', $3, $4, 'completed', 200, $5, $6, $7, $8)`,
			record.id, fixture.tokenID, record.key, bytes.Repeat([]byte{0x52}, 32),
			[]byte("response"), record.encrypted, now.Add(-time.Minute), now.Add(-time.Hour),
		); err != nil {
			t.Fatalf("insert idempotency record %s: %v", record.key, err)
		}
	}
	attemptID := uuid.MustParse("aaaaaaaa-9999-4999-8999-aaaaaaaaaaaa")
	insertPublishRecoveryAttempt(t, pool, fixture, attemptID, "3.0.0", "completed", now.Add(-time.Hour), now.Add(-time.Minute))
	if _, err := pool.Exec(t.Context(),
		"UPDATE publish_attempts SET idempotency_record_id = $1 WHERE id = $2", referencedID, attemptID,
	); err != nil {
		t.Fatalf("link publish idempotency record: %v", err)
	}
	cleaner := newRetentionCleaner(t, pool, now, &jobMemoryStore{objects: make(map[string][]byte)})
	if err := cleaner.CleanupIdempotencyOnce(t.Context()); err != nil {
		t.Fatalf("CleanupIdempotencyOnce() error = %v", err)
	}
	var records int
	if err := pool.QueryRow(t.Context(),
		"SELECT count(*) FROM idempotency_records WHERE id = ANY($1::uuid[])", []uuid.UUID{referencedID, standaloneID},
	).Scan(&records); err != nil {
		t.Fatalf("count expired idempotency records: %v", err)
	}
	var linkedID *uuid.UUID
	if err := pool.QueryRow(t.Context(), "SELECT idempotency_record_id FROM publish_attempts WHERE id = $1", attemptID).Scan(&linkedID); err != nil {
		t.Fatalf("load cleared publish attempt link: %v", err)
	}
	if records != 0 || linkedID != nil {
		t.Fatalf("cleanup result = records %d linked ID %v", records, linkedID)
	}
}

func newRetentionFixture(t *testing.T) (*pgxpool.Pool, uuid.UUID, uuid.UUID) {
	t.Helper()
	pool := testenv.StartPostgres(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	accountID := uuid.MustParse("aaaaaaaa-1111-4222-8333-bbbbbbbbbbbb")
	tokenID := uuid.MustParse("bbbbbbbb-2222-4333-8444-cccccccccccc")
	repositoryID := uuid.MustParse("cccccccc-3333-4444-8555-dddddddddddd")
	if _, err := pool.Exec(t.Context(), "INSERT INTO service_accounts (id, name) VALUES ($1, 'retention-worker')", accountID); err != nil {
		t.Fatalf("insert service account: %v", err)
	}
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO api_tokens
		 (id, service_account_id, secret_hmac, scopes, repository_ids, expires_at)
		 VALUES ($1, $2, $3, ARRAY['admin'], '{}', $4)`,
		tokenID, accountID, bytes.Repeat([]byte{0x11}, 32), time.Now().UTC().Add(24*time.Hour),
	); err != nil {
		t.Fatalf("insert API token: %v", err)
	}
	if _, err := pool.Exec(t.Context(),
		"INSERT INTO repositories (id, key, display_name, created_by) VALUES ($1, 'retention', 'Retention', $2)",
		repositoryID, tokenID,
	); err != nil {
		t.Fatalf("insert repository: %v", err)
	}
	return pool, tokenID, repositoryID
}

func newRetentionCleaner(t *testing.T, pool *pgxpool.Pool, now time.Time, store storage.Store) *Cleaner {
	t.Helper()
	return newRetentionCleanerWithClock(t, pool, clock.Fixed{Time: now}, store)
}

func newRetentionCleanerWithClock(t *testing.T, pool *pgxpool.Pool, jobClock clock.Clock, store storage.Store) *Cleaner {
	t.Helper()
	blobs, err := blob.NewService(blob.Options{Pool: pool, Clock: jobClock, Lease: time.Minute})
	if err != nil {
		t.Fatalf("NewService(blob) error = %v", err)
	}
	cleaner, err := NewCleaner(CleanupOptions{
		Pool: pool, Blobs: blobs, Store: store, Clock: jobClock, IDs: id.UUIDGenerator{},
		OrphanRetention: 24 * time.Hour, DeleteLease: time.Minute, BatchSize: 10,
	})
	if err != nil {
		t.Fatalf("NewCleaner() error = %v", err)
	}
	return cleaner
}

type jobMemoryStore struct {
	objects        map[string][]byte
	modified       map[string]time.Time
	deleteFailures int
}

func (s *jobMemoryStore) PutStaging(_ context.Context, key string, reader io.Reader, size int64) error {
	content, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	if int64(len(content)) != size {
		return storage.ErrObjectConflict
	}
	s.objects[key] = content
	return nil
}

func (s *jobMemoryStore) Promote(_ context.Context, stagingKey, objectKey string, _ int64) error {
	content, ok := s.objects[stagingKey]
	if !ok {
		return storage.ErrNotFound
	}
	s.objects[objectKey] = content
	delete(s.objects, stagingKey)
	return nil
}

func (s *jobMemoryStore) Open(_ context.Context, key, _ string) (storage.Object, error) {
	content, ok := s.objects[key]
	if !ok {
		return storage.Object{}, storage.ErrNotFound
	}
	reader := bytes.NewReader(content)
	return storage.Object{Body: io.NopCloser(reader), Seeker: reader, Info: storage.ObjectInfo{Key: key, Size: int64(len(content))}}, nil
}

func (s *jobMemoryStore) Stat(_ context.Context, key string) (storage.ObjectInfo, error) {
	content, ok := s.objects[key]
	if !ok {
		return storage.ObjectInfo{}, storage.ErrNotFound
	}
	return storage.ObjectInfo{Key: key, Size: int64(len(content)), LastModified: s.modified[key]}, nil
}

func (s *jobMemoryStore) List(_ context.Context, request storage.ListRequest) (storage.ListPage, error) {
	return listMemoryObjects(s.objects, s.modified, request)
}

func (s *jobMemoryStore) Delete(_ context.Context, key string) error {
	if s.deleteFailures > 0 {
		s.deleteFailures--
		return errors.New("injected object deletion failure")
	}
	delete(s.objects, key)
	return nil
}

func (s *jobMemoryStore) Presign(_ context.Context, key string, _ time.Duration) (string, error) {
	return "https://example.test/" + key, nil
}

func (*jobMemoryStore) Ready(context.Context) error { return nil }

func listMemoryObjects(objects map[string][]byte, modified map[string]time.Time, request storage.ListRequest) (storage.ListPage, error) {
	if request.Prefix == "" || request.Limit <= 0 {
		return storage.ListPage{}, errors.New("invalid list request")
	}
	keys := make([]string, 0, len(objects))
	for key := range objects {
		if strings.HasPrefix(key, request.Prefix) && key > request.After {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	page := storage.ListPage{Items: make([]storage.ObjectInfo, 0, min(len(keys), request.Limit))}
	for _, key := range keys {
		if len(page.Items) == request.Limit {
			break
		}
		lastModified := modified[key]
		if lastModified.IsZero() {
			lastModified = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
		}
		page.Items = append(page.Items, storage.ObjectInfo{
			Key: key, Size: int64(len(objects[key])), LastModified: lastModified,
		})
	}
	if len(page.Items) == request.Limit {
		page.NextAfter = page.Items[len(page.Items)-1].Key
	}
	return page, nil
}
