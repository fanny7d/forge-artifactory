package release

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"superfan.myasustor.com/fanchao/artifact-repository/internal/audit"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/auth"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/blob"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/clock"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/id"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/idempotency"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/signing"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/storage"
)

func TestExpiredPublisherCannotFinalizeAfterWorkerTakesLease(t *testing.T) {
	pool, draftService, actor, _ := newDraftTestService(t)
	release, err := draftService.Create(t.Context(), CreateDraftRequest{
		Mutation:      draftMutation(actor, "/api/v1/repositories/repo-a/packages/edgecli/releases", "create-fenced-publish"),
		RepositoryKey: "repo-a",
		PackageName:   "edgecli",
		Version:       "4.0.0",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	attemptID := uuid.MustParse("44444444-5555-4666-8777-888888888888")
	staleOwner := uuid.MustParse("55555555-6666-4777-8888-999999999999")
	workerOwner := uuid.MustParse("66666666-7777-4888-8999-aaaaaaaaaaaa")
	now := packageTestTime.Add(10 * time.Minute)
	keyID := "ed25519:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := pool.Exec(
		t.Context(),
		`INSERT INTO signing_keys (key_id, algorithm, public_key, fingerprint, active)
		 VALUES ($1, 'Ed25519', decode(repeat('aa', 32), 'hex'), repeat('a', 64), true)`,
		keyID,
	); err != nil {
		t.Fatalf("insert signing key: %v", err)
	}
	if _, err := pool.Exec(
		t.Context(),
		`INSERT INTO publish_attempts
		 (id, release_id, actor_token_id, request_id, published_at, snapshot, snapshot_sha256, key_id,
		  lease_owner, lease_generation, lease_expires_at, state)
		 VALUES ($1, $2, $3, 'request-fenced', $4, '{}', repeat('b', 64), $5, $6, 1, $7, 'active')`,
		attemptID,
		release.ID,
		actor.TokenID,
		packageTestTime,
		keyID,
		staleOwner,
		now.Add(-time.Minute),
	); err != nil {
		t.Fatalf("insert publish attempt: %v", err)
	}
	if _, err := pool.Exec(
		t.Context(),
		"UPDATE releases SET state = 'publishing', current_attempt_id = $1 WHERE id = $2",
		attemptID,
		release.ID,
	); err != nil {
		t.Fatalf("mark release publishing: %v", err)
	}

	store := newPublishAttemptStore(pool)
	stale := publishFence{AttemptID: attemptID, Owner: staleOwner, Generation: 1}
	worker, err := store.takeExpired(
		t.Context(),
		attemptID,
		workerOwner,
		now,
		now.Add(5*time.Minute),
	)
	if err != nil {
		t.Fatalf("takeExpired() error = %v", err)
	}
	if worker.Generation != stale.Generation+1 || worker.Owner != workerOwner {
		t.Fatalf("worker fence = %+v, stale fence = %+v", worker, stale)
	}

	workerFinalized := false
	if err := store.finalize(t.Context(), worker, now, func(context.Context, pgx.Tx) error {
		workerFinalized = true
		return nil
	}); err != nil {
		t.Fatalf("worker finalize() error = %v", err)
	}
	if !workerFinalized {
		t.Fatal("worker finalization callback was not called")
	}

	staleFinalized := false
	err = store.finalize(t.Context(), stale, now, func(context.Context, pgx.Tx) error {
		staleFinalized = true
		return nil
	})
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale finalize() error = %v, want ErrLeaseLost", err)
	}
	if staleFinalized {
		t.Fatal("stale finalization callback was called")
	}
}

func TestPublishPersistsSnapshotAndSignsManifest(t *testing.T) {
	pool, draftService, actor, _ := newDraftTestService(t)
	release, err := draftService.Create(t.Context(), CreateDraftRequest{
		Mutation:      draftMutation(actor, "/api/v1/repositories/repo-a/packages/edgecli/releases", "create-publish"),
		RepositoryKey: "repo-a",
		PackageName:   "edgecli",
		Version:       "5.0.0",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := draftService.AddArtifact(t.Context(), AddArtifactRequest{
		Mutation:      draftMutation(actor, "/api/v1/repositories/repo-a/packages/edgecli/releases/5.0.0/artifacts", "add-publish-artifact"),
		RepositoryKey: "repo-a",
		PackageName:   "edgecli",
		Version:       "5.0.0",
		ArtifactPath:  "linux/arm64/edgecli",
		OS:            "linux",
		Arch:          "arm64",
		Role:          "binary",
	}); err != nil {
		t.Fatalf("AddArtifact() error = %v", err)
	}

	seed := sha256.Sum256([]byte("publish-test-key"))
	signer, err := signing.NewEd25519(ed25519.NewKeyFromSeed(seed[:]))
	if err != nil {
		t.Fatalf("NewEd25519() error = %v", err)
	}
	store := &publishMemoryStore{objects: make(map[string][]byte)}
	fixedClock := clock.Fixed{Time: packageTestTime}
	blobs, err := blob.NewService(blob.Options{Pool: pool, Clock: fixedClock, Lease: time.Hour})
	if err != nil {
		t.Fatalf("NewService(blob) error = %v", err)
	}
	sealer, err := idempotency.NewSealer(bytes.Repeat([]byte{0x44}, 32), bytes.NewReader(bytes.Repeat([]byte{0x55}, 1024)))
	if err != nil {
		t.Fatalf("NewSealer() error = %v", err)
	}
	publisher, err := NewPublishService(PublishServiceOptions{
		Pool:           pool,
		Blobs:          blobs,
		Store:          store,
		Signer:         signer,
		Audit:          audit.NewService(pool),
		Idempotency:    idempotency.NewService(pool, sealer, fixedClock.Now),
		Clock:          fixedClock,
		IDs:            id.UUIDGenerator{},
		LeaseDuration:  5 * time.Minute,
		IdempotencyTTL: 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewPublishService() error = %v", err)
	}

	command := PublishCommand{
		Mutation:      draftMutation(actor, "/api/v1/repositories/repo-a/packages/edgecli/releases/5.0.0/publish", "publish-5.0.0"),
		RepositoryKey: "repo-a",
		PackageName:   "edgecli",
		Version:       "5.0.0",
	}
	published, err := publisher.Publish(t.Context(), command)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if published.Release.ID != release.ID || published.Release.State != "published" {
		t.Fatalf("published release = %+v", published.Release)
	}
	if len(published.Manifest) == 0 || len(published.Signature) != ed25519.SignatureSize {
		t.Fatalf("published output lengths = manifest %d, signature %d", len(published.Manifest), len(published.Signature))
	}
	if !ed25519.Verify(signer.PublicKey(), published.Manifest, published.Signature) {
		t.Fatal("published signature did not verify")
	}

	var state string
	var snapshot []byte
	var snapshotSHA string
	var attemptID uuid.UUID
	if err := pool.QueryRow(t.Context(),
		`SELECT r.state, p.snapshot, p.snapshot_sha256, p.id
		 FROM releases r JOIN publish_attempts p ON p.id = r.current_attempt_id
		 WHERE r.id = $1`, release.ID,
	).Scan(&state, &snapshot, &snapshotSHA, &attemptID); err != nil {
		t.Fatalf("load publish snapshot: %v", err)
	}
	if state != "published" || len(snapshot) == 0 || attemptID == uuid.Nil {
		t.Fatalf("publish persistence = state %q snapshot %q attempt %s", state, snapshot, attemptID)
	}
	canonicalPersistedSnapshot, err := canonicalizePersistedSnapshotForTest(snapshot)
	if err != nil {
		t.Fatalf("canonicalize persisted snapshot: %v", err)
	}
	snapshotDigest := sha256.Sum256(canonicalPersistedSnapshot)
	if snapshotSHA != hex.EncodeToString(snapshotDigest[:]) {
		t.Fatalf("snapshot sha = %s, want %s", snapshotSHA, hex.EncodeToString(snapshotDigest[:]))
	}

	var manifestSHA, signatureSHA string
	if err := pool.QueryRow(t.Context(),
		"SELECT manifest_blob_sha256, signature_blob_sha256 FROM release_manifests WHERE release_id = $1",
		release.ID,
	).Scan(&manifestSHA, &signatureSHA); err != nil {
		t.Fatalf("load release manifest: %v", err)
	}
	manifestDigest := sha256.Sum256(published.Manifest)
	signatureDigest := sha256.Sum256(published.Signature)
	if manifestSHA != hex.EncodeToString(manifestDigest[:]) || signatureSHA != hex.EncodeToString(signatureDigest[:]) {
		t.Fatalf("manifest references = %s/%s, want %s/%s", manifestSHA, signatureSHA, hex.EncodeToString(manifestDigest[:]), hex.EncodeToString(signatureDigest[:]))
	}
	if _, ok := store.objects[blob.ObjectKey(manifestSHA)]; !ok {
		t.Fatalf("manifest object %q was not stored", blob.ObjectKey(manifestSHA))
	}
	if _, ok := store.objects[blob.ObjectKey(signatureSHA)]; !ok {
		t.Fatalf("signature object %q was not stored", blob.ObjectKey(signatureSHA))
	}
}

func TestRecoverAttemptUsesPersistedSnapshotAndCompletesIdempotency(t *testing.T) {
	pool, draftService, actor, _ := newDraftTestService(t)
	release, err := draftService.Create(t.Context(), CreateDraftRequest{
		Mutation:      draftMutation(actor, "/api/v1/repositories/repo-a/packages/edgecli/releases", "create-recovery"),
		RepositoryKey: "repo-a",
		PackageName:   "edgecli",
		Version:       "6.0.0",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := draftService.AddArtifact(t.Context(), AddArtifactRequest{
		Mutation:      draftMutation(actor, "/api/v1/repositories/repo-a/packages/edgecli/releases/6.0.0/artifacts", "add-recovery-artifact"),
		RepositoryKey: "repo-a",
		PackageName:   "edgecli",
		Version:       "6.0.0",
		ArtifactPath:  "linux/arm64/edgecli",
		OS:            "linux",
		Arch:          "arm64",
		Role:          "binary",
	}); err != nil {
		t.Fatalf("AddArtifact() error = %v", err)
	}

	seed := sha256.Sum256([]byte("publish-recovery-key"))
	signer, err := signing.NewEd25519(ed25519.NewKeyFromSeed(seed[:]))
	if err != nil {
		t.Fatalf("NewEd25519() error = %v", err)
	}
	mutableClock := &publishMutableClock{now: packageTestTime}
	store := &publishMemoryStore{
		objects:             make(map[string][]byte),
		failPromotionSuffix: "/signature",
	}
	blobs, err := blob.NewService(blob.Options{Pool: pool, Clock: mutableClock, Lease: time.Minute})
	if err != nil {
		t.Fatalf("NewService(blob) error = %v", err)
	}
	sealer, err := idempotency.NewSealer(bytes.Repeat([]byte{0x46}, 32), bytes.NewReader(bytes.Repeat([]byte{0x57}, 1024)))
	if err != nil {
		t.Fatalf("NewSealer() error = %v", err)
	}
	publisher, err := NewPublishService(PublishServiceOptions{
		Pool:           pool,
		Blobs:          blobs,
		Store:          store,
		Signer:         signer,
		Audit:          audit.NewService(pool),
		Idempotency:    idempotency.NewService(pool, sealer, mutableClock.Now),
		Clock:          mutableClock,
		IDs:            id.UUIDGenerator{},
		LeaseDuration:  5 * time.Minute,
		IdempotencyTTL: 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewPublishService() error = %v", err)
	}
	command := PublishCommand{
		Mutation:      draftMutation(actor, "/api/v1/repositories/repo-a/packages/edgecli/releases/6.0.0/publish", "publish-6.0.0"),
		RepositoryKey: "repo-a",
		PackageName:   "edgecli",
		Version:       "6.0.0",
	}
	if _, err := publisher.Publish(t.Context(), command); err == nil {
		t.Fatal("Publish() succeeded despite injected signature storage failure")
	}

	var attemptID uuid.UUID
	var generation int64
	var releaseState, attemptState, idempotencyState string
	if err := pool.QueryRow(t.Context(),
		`SELECT p.id, p.lease_generation, r.state, p.state, i.state
		 FROM releases r
		 JOIN publish_attempts p ON p.id = r.current_attempt_id
		 JOIN idempotency_records i ON i.id = p.idempotency_record_id
		 WHERE r.id = $1`, release.ID,
	).Scan(&attemptID, &generation, &releaseState, &attemptState, &idempotencyState); err != nil {
		t.Fatalf("load failed publish attempt: %v", err)
	}
	if releaseState != "publishing" || attemptState != "active" || idempotencyState != "pending" || generation != 0 {
		t.Fatalf("failed publish state = release %q attempt %q idempotency %q generation %d", releaseState, attemptState, idempotencyState, generation)
	}

	mutableClock.now = mutableClock.now.Add(10 * time.Minute)
	recovered, err := publisher.RecoverAttempt(t.Context(), attemptID)
	if err != nil {
		t.Fatalf("RecoverAttempt() error = %v", err)
	}
	if recovered.Release.State != "published" || recovered.AttemptID != attemptID {
		t.Fatalf("recovered release = %+v", recovered)
	}
	if !ed25519.Verify(signer.PublicKey(), recovered.Manifest, recovered.Signature) {
		t.Fatal("recovered signature did not verify")
	}
	if err := pool.QueryRow(t.Context(),
		"SELECT lease_generation FROM publish_attempts WHERE id = $1", attemptID,
	).Scan(&generation); err != nil {
		t.Fatalf("load recovered generation: %v", err)
	}
	if generation != 1 {
		t.Fatalf("recovered generation = %d, want 1", generation)
	}

	replayed, err := publisher.Publish(t.Context(), command)
	if err != nil {
		t.Fatalf("Publish() replay error = %v", err)
	}
	if !replayed.Replayed || replayed.AttemptID != attemptID || !bytes.Equal(replayed.Manifest, recovered.Manifest) {
		t.Fatalf("replayed publish = %+v", replayed)
	}
}

func TestTransientPublishFailureRecordsRecoveryBackoff(t *testing.T) {
	mutableClock := &publishMutableClock{now: packageTestTime}
	store := &publishMemoryStore{
		objects:             make(map[string][]byte),
		failPromotionSuffix: "/signature",
	}
	fixture := newPublishFixture(t, "7.0.0", mutableClock, store, time.Minute)
	if _, err := fixture.publisher.Publish(t.Context(), fixture.command); !errors.Is(err, ErrPublishPending) {
		t.Fatalf("Publish() error = %v, want ErrPublishPending", err)
	}

	var failureCode *string
	var nextRetry *time.Time
	var retryCount int32
	var attemptState, releaseState string
	if err := fixture.pool.QueryRow(t.Context(),
		`SELECT p.failure_code, p.next_retry_at, p.retry_count, p.state, r.state
		 FROM publish_attempts p JOIN releases r ON r.current_attempt_id = p.id
		 WHERE r.id = $1`, fixture.release.ID,
	).Scan(&failureCode, &nextRetry, &retryCount, &attemptState, &releaseState); err != nil {
		t.Fatalf("load transient failure state: %v", err)
	}
	if failureCode == nil || *failureCode != "storage-transient" || nextRetry == nil || retryCount != 0 || attemptState != "active" || releaseState != "publishing" {
		t.Fatalf("transient failure state = code %v retry %v count %d attempt %q release %q", failureCode, nextRetry, retryCount, attemptState, releaseState)
	}
}

func TestPermanentSigningFailureMarksPublishFailedAndReplaysFailure(t *testing.T) {
	mutableClock := &publishMutableClock{now: packageTestTime}
	fixture := newPublishFixture(t, "8.0.0", mutableClock, &publishMemoryStore{objects: make(map[string][]byte)}, time.Minute)
	fixture.publisher.signer = &publishFailingSigner{Signer: fixture.publisher.signer, err: signing.ErrInvalidKey}
	if _, err := fixture.publisher.Publish(t.Context(), fixture.command); !errors.Is(err, ErrPublishFailed) {
		t.Fatalf("Publish() error = %v, want ErrPublishFailed", err)
	}

	var releaseState, attemptState, failureCode, idempotencyState string
	var httpStatus int32
	if err := fixture.pool.QueryRow(t.Context(),
		`SELECT r.state, p.state, p.failure_code, i.state, i.http_status
		 FROM releases r
		 JOIN publish_attempts p ON p.id = r.current_attempt_id
		 JOIN idempotency_records i ON i.id = p.idempotency_record_id
		 WHERE r.id = $1`, fixture.release.ID,
	).Scan(&releaseState, &attemptState, &failureCode, &idempotencyState, &httpStatus); err != nil {
		t.Fatalf("load permanent failure state: %v", err)
	}
	if releaseState != "publish_failed" || attemptState != "failed" || failureCode != "publish-integrity-failure" || idempotencyState != "completed" || httpStatus != 500 {
		t.Fatalf("permanent failure state = release %q attempt %q code %q idempotency %q status %d", releaseState, attemptState, failureCode, idempotencyState, httpStatus)
	}
	var auditCount int
	if err := fixture.pool.QueryRow(t.Context(),
		"SELECT count(*) FROM audit_events WHERE action = 'release.publish' AND outcome = 'failed' AND code = 'publish-integrity-failure' AND resource_id = $1",
		fixture.release.ID.String(),
	).Scan(&auditCount); err != nil {
		t.Fatalf("count failed publish audit: %v", err)
	}
	if auditCount != 1 {
		t.Fatalf("failed publish audit count = %d, want 1", auditCount)
	}
	if _, err := fixture.publisher.Publish(t.Context(), fixture.command); !errors.Is(err, ErrPublishFailed) {
		t.Fatalf("Publish() replay error = %v, want ErrPublishFailed", err)
	}
}

func TestPublishReportsBoundedSigningFailureCode(t *testing.T) {
	mutableClock := &publishMutableClock{now: packageTestTime}
	fixture := newPublishFixture(t, "8.1.0", mutableClock, &publishMemoryStore{objects: make(map[string][]byte)}, time.Minute)
	observer := &signingFailureObserver{}
	fixture.publisher.metrics = observer
	fixture.publisher.signer = &publishFailingSigner{Signer: fixture.publisher.signer, err: signing.ErrInvalidKey}
	if _, err := fixture.publisher.Publish(t.Context(), fixture.command); !errors.Is(err, ErrPublishFailed) {
		t.Fatalf("Publish() error = %v, want ErrPublishFailed", err)
	}
	if len(observer.codes) != 1 || observer.codes[0] != "invalid_key" {
		t.Fatalf("signing failure codes = %v, want [invalid_key]", observer.codes)
	}
}

func TestRetryLimitAbortsWhenFinalObjectsAreAbsent(t *testing.T) {
	mutableClock := &publishMutableClock{now: packageTestTime}
	store := &publishMemoryStore{
		objects:             make(map[string][]byte),
		failPromotionSuffix: "/manifest",
	}
	fixture := newPublishFixture(t, "9.0.0", mutableClock, store, time.Minute)
	if _, err := fixture.publisher.Publish(t.Context(), fixture.command); !errors.Is(err, ErrPublishPending) {
		t.Fatalf("Publish() error = %v, want ErrPublishPending", err)
	}
	var attemptID uuid.UUID
	if err := fixture.pool.QueryRow(t.Context(),
		"SELECT id FROM publish_attempts WHERE release_id = $1", fixture.release.ID,
	).Scan(&attemptID); err != nil {
		t.Fatalf("load attempt ID: %v", err)
	}
	if _, err := fixture.pool.Exec(t.Context(),
		"UPDATE publish_attempts SET retry_count = 9, next_retry_at = $1, lease_expires_at = $2 WHERE id = $3",
		mutableClock.now.Add(-time.Minute), mutableClock.now.Add(-time.Minute), attemptID,
	); err != nil {
		t.Fatalf("prepare retry limit: %v", err)
	}
	mutableClock.now = mutableClock.now.Add(10 * time.Minute)
	store.promotionFailed = false
	if _, err := fixture.publisher.RecoverAttempt(t.Context(), attemptID); !errors.Is(err, ErrPublishAborted) {
		t.Fatalf("RecoverAttempt() error = %v, want ErrPublishAborted", err)
	}

	var releaseState, attemptState, failureCode, idempotencyState string
	var httpStatus int32
	if err := fixture.pool.QueryRow(t.Context(),
		`SELECT r.state, p.state, p.failure_code, i.state, i.http_status
		 FROM releases r
		 JOIN publish_attempts p ON p.id = $1
		 JOIN idempotency_records i ON i.id = p.idempotency_record_id
		 WHERE r.id = $2`, attemptID, fixture.release.ID,
	).Scan(&releaseState, &attemptState, &failureCode, &idempotencyState, &httpStatus); err != nil {
		t.Fatalf("load aborted state: %v", err)
	}
	if releaseState != "draft" || attemptState != "aborted" || failureCode != "publish-attempt-aborted" || idempotencyState != "completed" || httpStatus != 503 {
		t.Fatalf("aborted state = release %q attempt %q code %q idempotency %q status %d", releaseState, attemptState, failureCode, idempotencyState, httpStatus)
	}
}

func TestRetryLimitMarksFailedWhenAnyFinalObjectExists(t *testing.T) {
	mutableClock := &publishMutableClock{now: packageTestTime}
	store := &publishMemoryStore{
		objects:             make(map[string][]byte),
		failPromotionSuffix: "/signature",
	}
	fixture := newPublishFixture(t, "9.1.0", mutableClock, store, time.Minute)
	if _, err := fixture.publisher.Publish(t.Context(), fixture.command); !errors.Is(err, ErrPublishPending) {
		t.Fatalf("Publish() error = %v, want ErrPublishPending", err)
	}
	var attemptID uuid.UUID
	if err := fixture.pool.QueryRow(t.Context(),
		"SELECT id FROM publish_attempts WHERE release_id = $1", fixture.release.ID,
	).Scan(&attemptID); err != nil {
		t.Fatalf("load attempt ID: %v", err)
	}
	if _, err := fixture.pool.Exec(t.Context(),
		"UPDATE publish_attempts SET retry_count = 9, next_retry_at = $1, lease_expires_at = $2 WHERE id = $3",
		mutableClock.now.Add(-time.Minute), mutableClock.now.Add(-time.Minute), attemptID,
	); err != nil {
		t.Fatalf("prepare retry limit: %v", err)
	}
	mutableClock.now = mutableClock.now.Add(10 * time.Minute)
	store.promotionFailed = false
	if _, err := fixture.publisher.RecoverAttempt(t.Context(), attemptID); !errors.Is(err, ErrPublishFailed) {
		t.Fatalf("RecoverAttempt() error = %v, want ErrPublishFailed", err)
	}
	var releaseState, attemptState, failureCode string
	if err := fixture.pool.QueryRow(t.Context(),
		`SELECT r.state, p.state, p.failure_code
		 FROM releases r JOIN publish_attempts p ON p.id = r.current_attempt_id
		 WHERE r.id = $1`, fixture.release.ID,
	).Scan(&releaseState, &attemptState, &failureCode); err != nil {
		t.Fatalf("load uncertain state: %v", err)
	}
	if releaseState != "publish_failed" || attemptState != "failed" || failureCode != "publish-integrity-uncertain" {
		t.Fatalf("uncertain state = release %q attempt %q code %q", releaseState, attemptState, failureCode)
	}
}

func TestRetryLimitMarksFailedWhenObjectAbsenceCannotBeProven(t *testing.T) {
	mutableClock := &publishMutableClock{now: packageTestTime}
	store := &publishMemoryStore{
		objects:             make(map[string][]byte),
		failPromotionSuffix: "/manifest",
	}
	fixture := newPublishFixture(t, "9.2.0", mutableClock, store, time.Minute)
	if _, err := fixture.publisher.Publish(t.Context(), fixture.command); !errors.Is(err, ErrPublishPending) {
		t.Fatalf("Publish() error = %v, want ErrPublishPending", err)
	}
	var attemptID uuid.UUID
	if err := fixture.pool.QueryRow(t.Context(), "SELECT id FROM publish_attempts WHERE release_id = $1", fixture.release.ID).Scan(&attemptID); err != nil {
		t.Fatalf("load attempt ID: %v", err)
	}
	if _, err := fixture.pool.Exec(t.Context(),
		"UPDATE publish_attempts SET retry_count = 9, next_retry_at = $1, lease_expires_at = $2 WHERE id = $3",
		mutableClock.now.Add(-time.Minute), mutableClock.now.Add(-time.Minute), attemptID,
	); err != nil {
		t.Fatalf("prepare retry limit: %v", err)
	}
	mutableClock.now = mutableClock.now.Add(10 * time.Minute)
	store.promotionFailed = false
	store.statErr = errors.New("injected HEAD failure")
	if _, err := fixture.publisher.RecoverAttempt(t.Context(), attemptID); !errors.Is(err, ErrPublishFailed) {
		t.Fatalf("RecoverAttempt() error = %v, want ErrPublishFailed", err)
	}
	var attemptState, failureCode string
	if err := fixture.pool.QueryRow(t.Context(),
		"SELECT state, failure_code FROM publish_attempts WHERE id = $1", attemptID,
	).Scan(&attemptState, &failureCode); err != nil {
		t.Fatalf("load uncertain HEAD state: %v", err)
	}
	if attemptState != "failed" || failureCode != "publish-integrity-uncertain" {
		t.Fatalf("uncertain HEAD state = attempt %q code %q", attemptState, failureCode)
	}
}

func TestPublishRetryBackoffCapsAtFiveMinutes(t *testing.T) {
	if got := publishRetryBackoff(0); got != 5*time.Second {
		t.Fatalf("backoff(0) = %s, want 5s", got)
	}
	if got := publishRetryBackoff(1); got != 10*time.Second {
		t.Fatalf("backoff(1) = %s, want 10s", got)
	}
	if got := publishRetryBackoff(20); got != 5*time.Minute {
		t.Fatalf("backoff(20) = %s, want 5m", got)
	}
}

func TestGetManifestReadsExactSignedObjectsForPublishedRelease(t *testing.T) {
	mutableClock := &publishMutableClock{now: packageTestTime}
	fixture := newPublishFixture(t, "10.0.0", mutableClock, &publishMemoryStore{objects: make(map[string][]byte)}, time.Minute)
	published, err := fixture.publisher.Publish(t.Context(), fixture.command)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	reader := fixture.actor
	reader.Scopes = auth.NewScopeSet(auth.ScopeArtifactRead)
	manifest, err := fixture.publisher.GetManifest(t.Context(), reader, "repo-a", "edgecli", "10.0.0")
	if err != nil {
		t.Fatalf("GetManifest() error = %v", err)
	}
	if manifest.Version != "10.0.0" || manifest.KeyID != published.KeyID || !bytes.Equal(manifest.Manifest, published.Manifest) || !bytes.Equal(manifest.Signature, published.Signature) {
		t.Fatalf("manifest = %+v", manifest)
	}
}

func TestRecoveryMarksTamperedSnapshotPublishFailed(t *testing.T) {
	mutableClock := &publishMutableClock{now: packageTestTime}
	store := &publishMemoryStore{
		objects:             make(map[string][]byte),
		failPromotionSuffix: "/manifest",
	}
	fixture := newPublishFixture(t, "11.0.0", mutableClock, store, time.Minute)
	if _, err := fixture.publisher.Publish(t.Context(), fixture.command); !errors.Is(err, ErrPublishPending) {
		t.Fatalf("Publish() error = %v, want ErrPublishPending", err)
	}
	var attemptID uuid.UUID
	if err := fixture.pool.QueryRow(t.Context(), "SELECT id FROM publish_attempts WHERE release_id = $1", fixture.release.ID).Scan(&attemptID); err != nil {
		t.Fatalf("load attempt ID: %v", err)
	}
	if _, err := fixture.pool.Exec(t.Context(),
		`UPDATE publish_attempts
		 SET snapshot = jsonb_set(snapshot, '{version}', '"tampered"'::jsonb),
		     lease_expires_at = $1,
		     next_retry_at = $1
		 WHERE id = $2`,
		mutableClock.now.Add(-time.Minute), attemptID,
	); err != nil {
		t.Fatalf("tamper publish snapshot: %v", err)
	}
	mutableClock.now = mutableClock.now.Add(10 * time.Minute)
	if _, err := fixture.publisher.RecoverAttempt(t.Context(), attemptID); !errors.Is(err, ErrPublishFailed) {
		t.Fatalf("RecoverAttempt() error = %v, want ErrPublishFailed", err)
	}
	var releaseState, attemptState, failureCode, idempotencyState string
	if err := fixture.pool.QueryRow(t.Context(),
		`SELECT r.state, p.state, p.failure_code, i.state
		 FROM releases r
		 JOIN publish_attempts p ON p.id = r.current_attempt_id
		 JOIN idempotency_records i ON i.id = p.idempotency_record_id
		 WHERE r.id = $1`, fixture.release.ID,
	).Scan(&releaseState, &attemptState, &failureCode, &idempotencyState); err != nil {
		t.Fatalf("load tampered recovery state: %v", err)
	}
	if releaseState != "publish_failed" || attemptState != "failed" || failureCode != "publish-integrity-failure" || idempotencyState != "completed" {
		t.Fatalf("tampered recovery state = release %q attempt %q code %q idempotency %q", releaseState, attemptState, failureCode, idempotencyState)
	}
}

func TestDeterministicValidationFailureCompletesIdempotencyWithoutAttempt(t *testing.T) {
	mutableClock := &publishMutableClock{now: packageTestTime}
	fixture := newPublishFixture(t, "12.0.0", mutableClock, &publishMemoryStore{objects: make(map[string][]byte)}, time.Minute)
	if _, err := fixture.pool.Exec(t.Context(), "DELETE FROM release_artifacts WHERE release_id = $1", fixture.release.ID); err != nil {
		t.Fatalf("remove fixture artifact: %v", err)
	}
	if _, err := fixture.publisher.Publish(t.Context(), fixture.command); !errors.Is(err, ErrNoArtifacts) {
		t.Fatalf("Publish() error = %v, want ErrNoArtifacts", err)
	}

	var state string
	var status int32
	if err := fixture.pool.QueryRow(t.Context(),
		`SELECT state, http_status
		 FROM idempotency_records
		 WHERE token_id = $1 AND canonical_resource = $2 AND idempotency_key = $3`,
		fixture.actor.TokenID,
		fixture.command.Mutation.CanonicalResource,
		fixture.command.Mutation.IdempotencyKey,
	).Scan(&state, &status); err != nil {
		t.Fatalf("load validation idempotency record: %v", err)
	}
	if state != "completed" || status != 422 {
		t.Fatalf("validation idempotency = state %q status %d", state, status)
	}
	var attempts int
	if err := fixture.pool.QueryRow(t.Context(), "SELECT count(*) FROM publish_attempts WHERE release_id = $1", fixture.release.ID).Scan(&attempts); err != nil {
		t.Fatalf("count publish attempts: %v", err)
	}
	if attempts != 0 {
		t.Fatalf("publish attempts = %d, want 0", attempts)
	}
	if _, err := fixture.publisher.Publish(t.Context(), fixture.command); !errors.Is(err, ErrNoArtifacts) {
		t.Fatalf("Publish() validation replay error = %v, want ErrNoArtifacts", err)
	}
}

func TestPublishHeartbeatsLeaseDuringSlowSigning(t *testing.T) {
	mutableClock := &publishMutableClock{now: packageTestTime}
	fixture := newPublishFixture(t, "13.0.0", mutableClock, &publishMemoryStore{objects: make(map[string][]byte)}, time.Minute)
	fixture.publisher.clock = clock.System{}
	fixture.publisher.heartbeatInterval = 20 * time.Millisecond
	blockingSigner := &publishBlockingSigner{
		Signer:  fixture.publisher.signer,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	fixture.publisher.signer = blockingSigner
	result := make(chan error, 1)
	go func() {
		_, err := fixture.publisher.Publish(t.Context(), fixture.command)
		result <- err
	}()
	released := false
	defer func() {
		if !released {
			close(blockingSigner.release)
		}
	}()
	select {
	case <-blockingSigner.entered:
	case <-time.After(time.Second):
		t.Fatal("Publish() did not reach signer")
	}

	var firstExpiry time.Time
	if err := fixture.pool.QueryRow(t.Context(),
		"SELECT lease_expires_at FROM publish_attempts WHERE release_id = $1", fixture.release.ID,
	).Scan(&firstExpiry); err != nil {
		t.Fatalf("load first lease expiry: %v", err)
	}
	advanced := false
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var currentExpiry time.Time
		if err := fixture.pool.QueryRow(t.Context(),
			"SELECT lease_expires_at FROM publish_attempts WHERE release_id = $1", fixture.release.ID,
		).Scan(&currentExpiry); err != nil {
			t.Fatalf("load heartbeat lease expiry: %v", err)
		}
		if currentExpiry.After(firstExpiry) {
			advanced = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !advanced {
		t.Fatal("publish lease expiry did not advance while signer was blocked")
	}
	close(blockingSigner.release)
	released = true
	if err := <-result; err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
}

func TestPublishHeartbeatErrorIgnoresRenewFailureAfterOperationStops(t *testing.T) {
	operationContext, cancel := context.WithCancel(t.Context())
	cancel()
	renewErr := errors.New("renew query observed canceled context")

	if err := publishHeartbeatError(operationContext, renewErr); err != nil {
		t.Fatalf("publishHeartbeatError() = %v, want nil after operation stop", err)
	}
	if err := publishHeartbeatError(t.Context(), renewErr); !errors.Is(err, renewErr) {
		t.Fatalf("publishHeartbeatError() = %v, want live renew error", err)
	}
}

func TestPublishDoesNotRecordStorageAfterCleanerFencesOutputBlob(t *testing.T) {
	mutableClock := &publishMutableClock{now: packageTestTime}
	store := &publishMemoryStore{
		objects:        make(map[string][]byte),
		blockPutSuffix: "/signature",
		putEntered:     make(chan struct{}),
		putRelease:     make(chan struct{}),
	}
	fixture := newPublishFixture(t, "13.1.0", mutableClock, store, time.Minute)

	result := make(chan error, 1)
	go func() {
		_, err := fixture.publisher.Publish(t.Context(), fixture.command)
		result <- err
	}()
	released := false
	defer func() {
		if !released {
			close(store.putRelease)
		}
	}()
	select {
	case <-store.putEntered:
	case <-time.After(time.Second):
		t.Fatal("Publish() did not reach signature storage")
	}

	var manifestSHA string
	if err := fixture.pool.QueryRow(t.Context(),
		`SELECT b.sha256
		 FROM blobs b
		 WHERE b.state = 'ready'
		   AND NOT EXISTS (SELECT 1 FROM artifacts a WHERE a.blob_sha256 = b.sha256)
		 ORDER BY b.created_at DESC
		 LIMIT 1`,
	).Scan(&manifestSHA); err != nil {
		t.Fatalf("load ready manifest blob: %v", err)
	}
	if _, err := fixture.publisher.blobs.BeginDelete(t.Context(), blob.DeleteRequest{
		SHA256: manifestSHA,
		Owner:  uuid.MustParse("77777777-7777-4777-8777-777777777777"),
	}); err != nil {
		t.Fatalf("fence manifest blob for deletion: %v", err)
	}

	close(store.putRelease)
	released = true
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("Publish() succeeded after its manifest Blob was fenced for deletion")
		}
	case <-time.After(time.Second):
		t.Fatal("Publish() did not return after signature storage was released")
	}

	var recordedManifestSHA, recordedSignatureSHA *string
	var storageCompleted bool
	if err := fixture.pool.QueryRow(t.Context(),
		`SELECT manifest_sha256, signature_sha256, storage_completed
		 FROM publish_attempts WHERE release_id = $1`,
		fixture.release.ID,
	).Scan(&recordedManifestSHA, &recordedSignatureSHA, &storageCompleted); err != nil {
		t.Fatalf("load publish storage state: %v", err)
	}
	if storageCompleted || recordedManifestSHA != nil || recordedSignatureSHA != nil {
		t.Fatalf("publish storage recorded after Blob fencing: completed=%t manifest=%v signature=%v", storageCompleted, recordedManifestSHA, recordedSignatureSHA)
	}
}

func canonicalizePersistedSnapshotForTest(encoded []byte) ([]byte, error) {
	var snapshot PublishSnapshot
	if err := json.Unmarshal(encoded, &snapshot); err != nil {
		return nil, err
	}
	return canonicalSnapshot(snapshot)
}

type publishMemoryStore struct {
	objects             map[string][]byte
	failPromotionSuffix string
	promotionFailed     bool
	statErr             error
	blockPutSuffix      string
	putEntered          chan struct{}
	putRelease          chan struct{}
}

func (s *publishMemoryStore) PutStaging(_ context.Context, key string, reader io.Reader, size int64) error {
	if key == "" || reader == nil || size < 0 {
		return fmt.Errorf("invalid staging request")
	}
	content, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	if int64(len(content)) != size {
		return fmt.Errorf("staging size %d, want %d", len(content), size)
	}
	if s.blockPutSuffix != "" && strings.HasSuffix(key, s.blockPutSuffix) {
		close(s.putEntered)
		<-s.putRelease
	}
	s.objects[key] = append([]byte(nil), content...)
	return nil
}

func (s *publishMemoryStore) Promote(_ context.Context, stagingKey, objectKey string, expectedSize int64) error {
	if !s.promotionFailed && s.failPromotionSuffix != "" && strings.HasSuffix(stagingKey, s.failPromotionSuffix) {
		s.promotionFailed = true
		return errors.New("injected promotion failure")
	}
	content, ok := s.objects[stagingKey]
	if !ok {
		return storage.ErrNotFound
	}
	if int64(len(content)) != expectedSize {
		return storage.ErrObjectConflict
	}
	s.objects[objectKey] = append([]byte(nil), content...)
	delete(s.objects, stagingKey)
	return nil
}

func (s *publishMemoryStore) Open(_ context.Context, key, _ string) (storage.Object, error) {
	content, ok := s.objects[key]
	if !ok {
		return storage.Object{}, storage.ErrNotFound
	}
	return storage.Object{Body: io.NopCloser(bytes.NewReader(content)), Seeker: bytes.NewReader(content), Info: storage.ObjectInfo{Key: key, Size: int64(len(content))}}, nil
}

func (s *publishMemoryStore) Stat(_ context.Context, key string) (storage.ObjectInfo, error) {
	if s.statErr != nil {
		return storage.ObjectInfo{}, s.statErr
	}
	content, ok := s.objects[key]
	if !ok {
		return storage.ObjectInfo{}, storage.ErrNotFound
	}
	return storage.ObjectInfo{Key: key, Size: int64(len(content))}, nil
}

func (*publishMemoryStore) List(context.Context, storage.ListRequest) (storage.ListPage, error) {
	return storage.ListPage{}, nil
}

func (s *publishMemoryStore) Delete(_ context.Context, key string) error {
	delete(s.objects, key)
	return nil
}

func (s *publishMemoryStore) Presign(_ context.Context, key string, _ time.Duration) (string, error) {
	return "https://example.test/" + key, nil
}

func (*publishMemoryStore) Ready(context.Context) error { return nil }

type publishMutableClock struct {
	now time.Time
}

func (c *publishMutableClock) Now() time.Time { return c.now }

type publishFailingSigner struct {
	signing.Signer
	err error
}

type signingFailureObserver struct {
	codes []string
}

func (o *signingFailureObserver) ObserveSigningFailure(code string) {
	o.codes = append(o.codes, code)
}

func (s *publishFailingSigner) Sign(context.Context, []byte) ([]byte, error) {
	return nil, s.err
}

type publishBlockingSigner struct {
	signing.Signer
	entered chan struct{}
	release chan struct{}
}

func (s *publishBlockingSigner) Sign(ctx context.Context, message []byte) ([]byte, error) {
	close(s.entered)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.release:
		return s.Signer.Sign(ctx, message)
	}
}

type publishFixture struct {
	pool      *pgxpool.Pool
	release   Release
	actor     auth.Actor
	publisher *PublishService
	command   PublishCommand
}

func newPublishFixture(t *testing.T, version string, mutableClock *publishMutableClock, store *publishMemoryStore, blobLease time.Duration) publishFixture {
	t.Helper()
	pool, draftService, actor, _ := newDraftTestService(t)
	release, err := draftService.Create(t.Context(), CreateDraftRequest{
		Mutation:      draftMutation(actor, "/api/v1/repositories/repo-a/packages/edgecli/releases", "create-"+version),
		RepositoryKey: "repo-a",
		PackageName:   "edgecli",
		Version:       version,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := draftService.AddArtifact(t.Context(), AddArtifactRequest{
		Mutation:      draftMutation(actor, "/api/v1/repositories/repo-a/packages/edgecli/releases/"+version+"/artifacts", "add-"+version),
		RepositoryKey: "repo-a",
		PackageName:   "edgecli",
		Version:       version,
		ArtifactPath:  "linux/arm64/edgecli",
		OS:            "linux",
		Arch:          "arm64",
		Role:          "binary",
	}); err != nil {
		t.Fatalf("AddArtifact() error = %v", err)
	}
	seed := sha256.Sum256([]byte("publish-fixture-key:" + version))
	signer, err := signing.NewEd25519(ed25519.NewKeyFromSeed(seed[:]))
	if err != nil {
		t.Fatalf("NewEd25519() error = %v", err)
	}
	blobs, err := blob.NewService(blob.Options{Pool: pool, Clock: mutableClock, Lease: blobLease})
	if err != nil {
		t.Fatalf("NewService(blob) error = %v", err)
	}
	sealer, err := idempotency.NewSealer(bytes.Repeat([]byte{0x66}, 32), bytes.NewReader(bytes.Repeat([]byte{0x77}, 1024)))
	if err != nil {
		t.Fatalf("NewSealer() error = %v", err)
	}
	publisher, err := NewPublishService(PublishServiceOptions{
		Pool:           pool,
		Blobs:          blobs,
		Store:          store,
		Signer:         signer,
		Audit:          audit.NewService(pool),
		Idempotency:    idempotency.NewService(pool, sealer, mutableClock.Now),
		Clock:          mutableClock,
		IDs:            id.UUIDGenerator{},
		LeaseDuration:  5 * time.Minute,
		IdempotencyTTL: 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewPublishService() error = %v", err)
	}
	return publishFixture{
		pool:      pool,
		release:   release,
		actor:     actor,
		publisher: publisher,
		command: PublishCommand{
			Mutation:      draftMutation(actor, "/api/v1/repositories/repo-a/packages/edgecli/releases/"+version+"/publish", "publish-"+version),
			RepositoryKey: "repo-a",
			PackageName:   "edgecli",
			Version:       version,
		},
	}
}
