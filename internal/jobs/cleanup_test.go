package jobs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"superfan.myasustor.com/fanchao/artifact-repository/internal/blob"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/clock"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/database"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/id"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/storage"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/testenv"
)

func TestCleanupBlobsFencesNewReferencesBeforeObjectDelete(t *testing.T) {
	pool := testenv.StartPostgres(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	blobs, err := blob.NewService(blob.Options{Pool: pool, Clock: clock.Fixed{Time: now}, Lease: time.Minute})
	if err != nil {
		t.Fatalf("NewService(blob) error = %v", err)
	}
	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	owner := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	claim, err := blobs.Claim(t.Context(), blob.ClaimRequest{SHA256: sha, Size: 7, Owner: owner})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if _, err := blobs.MarkReady(t.Context(), blob.Fence{SHA256: sha, Owner: owner, Generation: claim.Generation}); err != nil {
		t.Fatalf("MarkReady() error = %v", err)
	}
	store := &blockingDeleteStore{
		objects: map[string][]byte{claim.ObjectKey: []byte("payload")},
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	cleanupClock := &runnerMutableClock{now: now.Add(48 * time.Hour)}
	cleaner, err := NewCleaner(CleanupOptions{
		Pool:            pool,
		Blobs:           blobs,
		Store:           store,
		Clock:           cleanupClock,
		IDs:             id.UUIDGenerator{},
		OrphanRetention: 24 * time.Hour,
		BatchSize:       10,
		DeleteLease:     time.Minute,
	})
	if err != nil {
		t.Fatalf("NewCleaner() error = %v", err)
	}
	result := make(chan error, 1)
	go func() { result <- cleaner.CleanupBlobsOnce(t.Context()) }()
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("cleaner did not reach object deletion")
	}

	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin reference transaction: %v", err)
	}
	if _, err := blobs.Reference(t.Context(), tx, sha); !errors.Is(err, blob.ErrDeleting) {
		_ = tx.Rollback(t.Context())
		t.Fatalf("Reference() error = %v, want ErrDeleting", err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatalf("rollback reference transaction: %v", err)
	}
	close(store.release)
	if err := <-result; err != nil {
		t.Fatalf("CleanupBlobsOnce() error = %v", err)
	}
	var state string
	var deleteCompletedAt *time.Time
	if err := pool.QueryRow(t.Context(), "SELECT state, delete_completed_at FROM blobs WHERE sha256 = $1", sha).Scan(&state, &deleteCompletedAt); err != nil {
		t.Fatalf("load quarantined blob: %v", err)
	}
	if state != "deleting" || deleteCompletedAt == nil {
		t.Fatalf("quarantined blob = state %q completedAt %v", state, deleteCompletedAt)
	}
	cleanupClock.Advance(2 * time.Minute)
	if err := cleaner.CleanupBlobsOnce(t.Context()); err != nil {
		t.Fatalf("CleanupBlobsOnce(finalize) error = %v", err)
	}
	var count int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM blobs WHERE sha256 = $1", sha).Scan(&count); err != nil {
		t.Fatalf("count cleaned blob: %v", err)
	}
	if count != 0 {
		t.Fatalf("cleaned blob rows = %d, want 0", count)
	}
}

func TestCleanupBlobsReconcilesRowlessObjectWithTombstoneBeforeDelete(t *testing.T) {
	pool := testenv.StartPostgres(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	blobs, err := blob.NewService(blob.Options{Pool: pool, Clock: clock.Fixed{Time: now}, Lease: time.Minute})
	if err != nil {
		t.Fatalf("NewService(blob) error = %v", err)
	}
	sha := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	key := blob.ObjectKey(sha)
	store := &blockingDeleteStore{
		objects:  map[string][]byte{key: []byte("rowless")},
		modified: map[string]time.Time{key: now.Add(-48 * time.Hour)},
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	cleaner, err := NewCleaner(CleanupOptions{
		Pool: pool, Blobs: blobs, Store: store, Clock: clock.Fixed{Time: now}, IDs: id.UUIDGenerator{},
		OrphanRetention: 24 * time.Hour, DeleteLease: time.Minute,
		DeleteHeartbeat: 10 * time.Second, DeleteTimeout: 2 * time.Minute,
		DeleteQuarantine: time.Minute, BatchSize: 1,
	})
	if err != nil {
		t.Fatalf("NewCleaner() error = %v", err)
	}

	result := make(chan error, 1)
	go func() { result <- cleaner.CleanupBlobsOnce(t.Context()) }()
	released := false
	defer func() {
		if !released {
			close(store.release)
		}
	}()
	select {
	case <-store.entered:
	case err := <-result:
		t.Fatalf("CleanupBlobsOnce() returned before deleting rowless object: %v", err)
	case <-time.After(time.Second):
		t.Fatal("CleanupBlobsOnce() did not reach rowless object deletion")
	}

	var state string
	var generation int64
	var deleteCompletedAt *time.Time
	if err := pool.QueryRow(t.Context(),
		"SELECT state, lease_generation, delete_completed_at FROM blobs WHERE sha256 = $1", sha,
	).Scan(&state, &generation, &deleteCompletedAt); err != nil {
		t.Fatalf("load rowless Blob tombstone: %v", err)
	}
	if state != "deleting" || generation != 0 || deleteCompletedAt != nil {
		t.Fatalf("rowless Blob tombstone = state %q generation %d completedAt %v", state, generation, deleteCompletedAt)
	}
	if _, err := blobs.Claim(t.Context(), blob.ClaimRequest{SHA256: sha, Size: 7, Owner: uuid.New()}); !errors.Is(err, blob.ErrDeleting) {
		t.Fatalf("Claim(rowless tombstone) error = %v, want ErrDeleting", err)
	}

	close(store.release)
	released = true
	if err := <-result; err != nil {
		t.Fatalf("CleanupBlobsOnce() error = %v", err)
	}
	if _, exists := store.objects[key]; exists {
		t.Fatal("rowless object still exists after cleanup")
	}
	if err := pool.QueryRow(t.Context(),
		"SELECT state, delete_completed_at FROM blobs WHERE sha256 = $1", sha,
	).Scan(&state, &deleteCompletedAt); err != nil {
		t.Fatalf("load completed rowless Blob tombstone: %v", err)
	}
	if state != "deleting" || deleteCompletedAt == nil {
		t.Fatalf("completed rowless Blob tombstone = state %q completedAt %v", state, deleteCompletedAt)
	}
}

func TestCleanupBlobsAdvancesBoundedStorageListing(t *testing.T) {
	pool := testenv.StartPostgres(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	blobs, err := blob.NewService(blob.Options{Pool: pool, Clock: clock.Fixed{Time: now}, Lease: time.Minute})
	if err != nil {
		t.Fatalf("NewService(blob) error = %v", err)
	}
	trackedSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	orphanSHA := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	owner := uuid.MustParse("33333333-3333-4333-8333-333333333333")
	claim, err := blobs.Claim(t.Context(), blob.ClaimRequest{SHA256: trackedSHA, Size: 7, Owner: owner})
	if err != nil {
		t.Fatalf("Claim(tracked) error = %v", err)
	}
	if _, err := blobs.MarkReady(t.Context(), blob.Fence{SHA256: trackedSHA, Owner: owner, Generation: claim.Generation}); err != nil {
		t.Fatalf("MarkReady(tracked) error = %v", err)
	}
	trackedKey := blob.ObjectKey(trackedSHA)
	orphanKey := blob.ObjectKey(orphanSHA)
	store := &jobMemoryStore{
		objects: map[string][]byte{trackedKey: []byte("tracked"), orphanKey: []byte("orphan")},
		modified: map[string]time.Time{
			trackedKey: now.Add(-48 * time.Hour),
			orphanKey:  now.Add(-48 * time.Hour),
		},
	}
	cleaner, err := NewCleaner(CleanupOptions{
		Pool: pool, Blobs: blobs, Store: store, Clock: clock.Fixed{Time: now}, IDs: id.UUIDGenerator{},
		OrphanRetention: 24 * time.Hour, DeleteLease: time.Minute, BatchSize: 1,
	})
	if err != nil {
		t.Fatalf("NewCleaner() error = %v", err)
	}
	if err := cleaner.CleanupBlobsOnce(t.Context()); err != nil {
		t.Fatalf("CleanupBlobsOnce(first page) error = %v", err)
	}
	if _, exists := store.objects[orphanKey]; !exists {
		t.Fatal("orphan from second page was deleted during first bounded listing")
	}
	if err := cleaner.CleanupBlobsOnce(t.Context()); err != nil {
		t.Fatalf("CleanupBlobsOnce(second page) error = %v", err)
	}
	if _, exists := store.objects[trackedKey]; !exists {
		t.Fatal("tracked Blob object was deleted during storage reconciliation")
	}
	if _, exists := store.objects[orphanKey]; exists {
		t.Fatal("rowless Blob object from second page still exists")
	}
}

func TestCleanupBlobsRetainsLeaseAfterObjectDeleteFailure(t *testing.T) {
	pool := testenv.StartPostgres(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	createdAt := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	cleanupAt := createdAt.Add(48 * time.Hour)
	blobs, err := blob.NewService(blob.Options{Pool: pool, Clock: clock.Fixed{Time: createdAt}, Lease: time.Minute})
	if err != nil {
		t.Fatalf("NewService(blob) error = %v", err)
	}
	sha := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	owner := uuid.MustParse("88888888-8888-4888-8888-888888888888")
	claim, err := blobs.Claim(t.Context(), blob.ClaimRequest{SHA256: sha, Size: 7, Owner: owner})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if _, err := blobs.MarkReady(t.Context(), blob.Fence{SHA256: sha, Owner: owner, Generation: claim.Generation}); err != nil {
		t.Fatalf("MarkReady() error = %v", err)
	}
	store := &jobMemoryStore{
		objects:        map[string][]byte{claim.ObjectKey: []byte("payload")},
		deleteFailures: 1,
	}
	cleanupClock := &runnerMutableClock{now: cleanupAt}
	cleaner := newRetentionCleanerWithClock(t, pool, cleanupClock, store)
	if err := cleaner.CleanupBlobsOnce(t.Context()); err == nil {
		t.Fatal("CleanupBlobsOnce(first) error = nil, want injected deletion failure")
	}
	var state string
	var generation int64
	var leaseExpiresAt time.Time
	if err := pool.QueryRow(t.Context(),
		"SELECT state, lease_generation, lease_expires_at FROM blobs WHERE sha256 = $1", sha,
	).Scan(&state, &generation, &leaseExpiresAt); err != nil {
		t.Fatalf("load failed blob cleanup lease: %v", err)
	}
	if state != "deleting" || generation != 1 || !leaseExpiresAt.After(cleanupAt) {
		t.Fatalf("failed blob cleanup lease = state %q generation %d expiry %s", state, generation, leaseExpiresAt)
	}
	if err := cleaner.CleanupBlobsOnce(t.Context()); err != nil {
		t.Fatalf("CleanupBlobsOnce(before lease expiry) error = %v", err)
	}
	if _, exists := store.objects[claim.ObjectKey]; !exists {
		t.Fatal("blob object deleted before retry lease expired")
	}
	cleanupClock.Advance(2 * time.Minute)
	if err := cleaner.CleanupBlobsOnce(t.Context()); err != nil {
		t.Fatalf("CleanupBlobsOnce(retry) error = %v", err)
	}
	if err := pool.QueryRow(t.Context(), "SELECT state, lease_generation FROM blobs WHERE sha256 = $1", sha).Scan(&state, &generation); err != nil {
		t.Fatalf("load retried blob quarantine: %v", err)
	}
	if state != "deleting" || generation != 2 {
		t.Fatalf("retried blob quarantine = state %q generation %d", state, generation)
	}
	cleanupClock.Advance(2 * time.Minute)
	if err := cleaner.CleanupBlobsOnce(t.Context()); err != nil {
		t.Fatalf("CleanupBlobsOnce(finalize retry) error = %v", err)
	}
	var count int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM blobs WHERE sha256 = $1", sha).Scan(&count); err != nil {
		t.Fatalf("count retried blob cleanup: %v", err)
	}
	if count != 0 {
		t.Fatalf("retried blob rows = %d, want 0", count)
	}
}

func TestCleanupBlobsRetainsActivePublishAttemptOutputs(t *testing.T) {
	pool, fixture := newPublishRecoveryFixture(t)
	createdAt := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	cleanupAt := createdAt.Add(48 * time.Hour)
	blobs, err := blob.NewService(blob.Options{Pool: pool, Clock: clock.Fixed{Time: createdAt}, Lease: time.Minute})
	if err != nil {
		t.Fatalf("NewService(blob) error = %v", err)
	}
	sha := "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	owner := uuid.MustParse("99999999-9999-4999-8999-999999999999")
	claim, err := blobs.Claim(t.Context(), blob.ClaimRequest{SHA256: sha, Size: 7, Owner: owner})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if _, err := blobs.MarkReady(t.Context(), blob.Fence{SHA256: sha, Owner: owner, Generation: claim.Generation}); err != nil {
		t.Fatalf("MarkReady() error = %v", err)
	}
	attemptID := uuid.MustParse("bbbbbbbb-9999-4999-8999-bbbbbbbbbbbb")
	insertPublishRecoveryAttempt(t, pool, fixture, attemptID, "4.0.0", "active", cleanupAt.Add(time.Hour), cleanupAt.Add(time.Hour))
	if _, err := pool.Exec(t.Context(),
		`UPDATE publish_attempts
		 SET storage_completed = true, manifest_sha256 = $1, signature_sha256 = $1
		 WHERE id = $2`, sha, attemptID,
	); err != nil {
		t.Fatalf("record active publish outputs: %v", err)
	}
	store := &jobMemoryStore{objects: map[string][]byte{claim.ObjectKey: []byte("payload")}}
	cleaner := newRetentionCleaner(t, pool, cleanupAt, store)
	if err := cleaner.CleanupBlobsOnce(t.Context()); err != nil {
		t.Fatalf("CleanupBlobsOnce() error = %v", err)
	}
	var count int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM blobs WHERE sha256 = $1", sha).Scan(&count); err != nil {
		t.Fatalf("count retained publish blob: %v", err)
	}
	if _, exists := store.objects[claim.ObjectKey]; count != 1 || !exists {
		t.Fatalf("active publish blob = rows %d objectExists %t", count, exists)
	}
}

func TestCleanupBlobsHeartbeatsAndQuarantinesSlowDelete(t *testing.T) {
	pool := testenv.StartPostgres(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	createdAt := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	cleanupClock := &runnerMutableClock{now: createdAt.Add(48 * time.Hour)}
	blobs, err := blob.NewService(blob.Options{Pool: pool, Clock: cleanupClock, Lease: time.Minute})
	if err != nil {
		t.Fatalf("NewService(blob) error = %v", err)
	}
	sha := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	owner := uuid.MustParse("aaaaaaaa-8888-4888-8888-aaaaaaaaaaaa")
	claim, err := blobs.Claim(t.Context(), blob.ClaimRequest{SHA256: sha, Size: 7, Owner: owner})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if _, err := blobs.MarkReady(t.Context(), blob.Fence{SHA256: sha, Owner: owner, Generation: claim.Generation}); err != nil {
		t.Fatalf("MarkReady() error = %v", err)
	}
	store := &leaseDeleteStore{
		jobMemoryStore: &jobMemoryStore{objects: map[string][]byte{claim.ObjectKey: []byte("payload")}},
		firstEntered:   make(chan struct{}),
		secondEntered:  make(chan struct{}),
		releaseFirst:   make(chan struct{}),
		releaseSecond:  make(chan struct{}),
	}
	newCleaner := func() *Cleaner {
		cleaner, err := NewCleaner(CleanupOptions{
			Pool: pool, Blobs: blobs, Store: store, Clock: cleanupClock, IDs: id.UUIDGenerator{},
			OrphanRetention: 24 * time.Hour, DeleteLease: 200 * time.Millisecond,
			DeleteHeartbeat: 20 * time.Millisecond, DeleteTimeout: time.Second,
			DeleteQuarantine: time.Minute, BatchSize: 1,
		})
		if err != nil {
			t.Fatalf("NewCleaner() error = %v", err)
		}
		return cleaner
	}
	firstResult := make(chan error, 1)
	go func() { firstResult <- newCleaner().CleanupBlobsOnce(t.Context()) }()
	select {
	case <-store.firstEntered:
	case <-time.After(time.Second):
		t.Fatal("first cleaner did not enter object deletion")
	}
	originalExpiry := cleanupClock.Now().Add(200 * time.Millisecond)
	cleanupClock.Advance(time.Minute)
	deadline := time.After(time.Second)
	for {
		var leaseExpiresAt time.Time
		if err := pool.QueryRow(t.Context(), "SELECT lease_expires_at FROM blobs WHERE sha256 = $1", sha).Scan(&leaseExpiresAt); err != nil {
			t.Fatalf("load heartbeat lease: %v", err)
		}
		if leaseExpiresAt.After(cleanupClock.Now()) && leaseExpiresAt.After(originalExpiry) {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("blob delete lease was not heartbeated: %s", leaseExpiresAt)
		case <-time.After(10 * time.Millisecond):
		}
	}
	secondResult := make(chan error, 1)
	go func() { secondResult <- newCleaner().CleanupBlobsOnce(t.Context()) }()
	select {
	case <-store.secondEntered:
		close(store.releaseSecond)
		close(store.releaseFirst)
		<-firstResult
		<-secondResult
		t.Fatal("second cleaner took over a heartbeated delete lease")
	case err := <-secondResult:
		if err != nil {
			close(store.releaseFirst)
			<-firstResult
			t.Fatalf("second CleanupBlobsOnce() error = %v", err)
		}
	case <-time.After(time.Second):
		close(store.releaseFirst)
		<-firstResult
		t.Fatal("second cleaner did not finish")
	}
	close(store.releaseFirst)
	if err := <-firstResult; err != nil {
		t.Fatalf("first CleanupBlobsOnce() error = %v", err)
	}
	var state string
	var deleteCompletedAt *time.Time
	if err := pool.QueryRow(t.Context(),
		"SELECT state, delete_completed_at FROM blobs WHERE sha256 = $1", sha,
	).Scan(&state, &deleteCompletedAt); err != nil {
		t.Fatalf("load quarantined blob: %v", err)
	}
	if state != "deleting" || deleteCompletedAt == nil {
		t.Fatalf("quarantined blob = state %q completedAt %v", state, deleteCompletedAt)
	}
	if _, err := blobs.Claim(t.Context(), blob.ClaimRequest{SHA256: sha, Size: 7, Owner: uuid.New()}); !errors.Is(err, blob.ErrDeleting) {
		t.Fatalf("Claim(quarantined) error = %v, want ErrDeleting", err)
	}
	cleanupClock.Advance(2 * time.Minute)
	if err := newCleaner().CleanupBlobsOnce(t.Context()); err != nil {
		t.Fatalf("CleanupBlobsOnce(finalize quarantine) error = %v", err)
	}
	var count int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM blobs WHERE sha256 = $1", sha).Scan(&count); err != nil {
		t.Fatalf("count finalized tombstone: %v", err)
	}
	if count != 0 {
		t.Fatalf("finalized tombstone rows = %d, want 0", count)
	}
}

func TestHeartbeatErrorIgnoresQueryFailureAfterOperationStops(t *testing.T) {
	operationContext, cancel := context.WithCancel(t.Context())
	cancel()
	queryErr := errors.New("renew query observed canceled context")

	if err := heartbeatError(operationContext, queryErr); err != nil {
		t.Fatalf("heartbeatError() = %v, want nil after operation stop", err)
	}
	liveContext := t.Context()
	if err := heartbeatError(liveContext, queryErr); !errors.Is(err, queryErr) {
		t.Fatalf("heartbeatError() = %v, want live query error", err)
	}
}

type leaseDeleteStore struct {
	*jobMemoryStore
	mu            sync.Mutex
	deleteCalls   int
	firstEntered  chan struct{}
	secondEntered chan struct{}
	releaseFirst  chan struct{}
	releaseSecond chan struct{}
}

func (s *leaseDeleteStore) Delete(ctx context.Context, key string) error {
	s.mu.Lock()
	s.deleteCalls++
	call := s.deleteCalls
	s.mu.Unlock()
	var entered, release chan struct{}
	switch call {
	case 1:
		entered, release = s.firstEntered, s.releaseFirst
	case 2:
		entered, release = s.secondEntered, s.releaseSecond
	default:
		return s.jobMemoryStore.Delete(ctx, key)
	}
	close(entered)
	select {
	case <-release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.jobMemoryStore.Delete(ctx, key)
}

type blockingDeleteStore struct {
	objects  map[string][]byte
	modified map[string]time.Time
	entered  chan struct{}
	release  chan struct{}
}

func (s *blockingDeleteStore) PutStaging(_ context.Context, key string, reader io.Reader, size int64) error {
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

func (s *blockingDeleteStore) Promote(_ context.Context, stagingKey, objectKey string, _ int64) error {
	content, ok := s.objects[stagingKey]
	if !ok {
		return storage.ErrNotFound
	}
	s.objects[objectKey] = content
	delete(s.objects, stagingKey)
	return nil
}

func (s *blockingDeleteStore) Open(_ context.Context, key, _ string) (storage.Object, error) {
	content, ok := s.objects[key]
	if !ok {
		return storage.Object{}, storage.ErrNotFound
	}
	reader := bytes.NewReader(content)
	return storage.Object{Body: io.NopCloser(reader), Seeker: reader, Info: storage.ObjectInfo{Key: key, Size: int64(len(content))}}, nil
}

func (s *blockingDeleteStore) Stat(_ context.Context, key string) (storage.ObjectInfo, error) {
	content, ok := s.objects[key]
	if !ok {
		return storage.ObjectInfo{}, storage.ErrNotFound
	}
	return storage.ObjectInfo{Key: key, Size: int64(len(content)), LastModified: s.modified[key]}, nil
}

func (s *blockingDeleteStore) List(_ context.Context, request storage.ListRequest) (storage.ListPage, error) {
	return listMemoryObjects(s.objects, s.modified, request)
}

func (s *blockingDeleteStore) Delete(ctx context.Context, key string) error {
	select {
	case <-s.entered:
	default:
		close(s.entered)
	}
	select {
	case <-s.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	delete(s.objects, key)
	return nil
}

func (s *blockingDeleteStore) Presign(_ context.Context, key string, _ time.Duration) (string, error) {
	return "https://example.test/" + key, nil
}

func (*blockingDeleteStore) Ready(context.Context) error { return nil }
