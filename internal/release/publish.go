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
	"time"

	"github.com/google/uuid"
	"github.com/gowebpki/jcs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"superfan.myasustor.com/fanchao/artifact-repository/internal/audit"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/auth"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/blob"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/clock"
	db "superfan.myasustor.com/fanchao/artifact-repository/internal/database/sqlc"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/id"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/idempotency"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/signing"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/storage"
)

var (
	ErrLeaseLost       = errors.New("publish lease lost")
	ErrNoArtifacts     = errors.New("release has no artifacts")
	ErrPublishNotReady = errors.New("release is not ready to publish")
	ErrPublishPending  = errors.New("publish attempt is pending recovery")
	ErrPublishFailed   = errors.New("publish attempt failed")
	ErrPublishAborted  = errors.New("publish attempt aborted")
)

type PublishServiceOptions struct {
	Pool           *pgxpool.Pool
	Blobs          *blob.Service
	Store          storage.Store
	Signer         signing.Signer
	Audit          *audit.Service
	Idempotency    *idempotency.Service
	Clock          clock.Clock
	IDs            id.Generator
	LeaseDuration  time.Duration
	Heartbeat      time.Duration
	IdempotencyTTL time.Duration
	Metrics        SigningMetrics
}

type SigningMetrics interface {
	ObserveSigningFailure(string)
}

type PublishService struct {
	pool              *pgxpool.Pool
	blobs             *blob.Service
	store             storage.Store
	signer            signing.Signer
	audit             *audit.Service
	idempotency       *idempotency.Service
	clock             clock.Clock
	ids               id.Generator
	leaseDuration     time.Duration
	heartbeatInterval time.Duration
	idempotencyTTL    time.Duration
	metrics           SigningMetrics
	attempts          *publishAttemptStore
}

type PublishCommand struct {
	Mutation      auth.Mutation
	RepositoryKey string
	PackageName   string
	Version       string
}

type PublishedRelease struct {
	Release         Release   `json:"release"`
	Manifest        []byte    `json:"manifest"`
	Signature       []byte    `json:"signature"`
	ManifestSHA256  string    `json:"manifestSha256"`
	SignatureSHA256 string    `json:"signatureSha256"`
	KeyID           string    `json:"keyId"`
	AttemptID       uuid.UUID `json:"attemptId"`
	Replayed        bool      `json:"-"`
}

type SignedManifest struct {
	Version         string
	Manifest        []byte
	ManifestSHA256  string
	KeyID           string
	Signature       []byte
	SignatureSHA256 string
}

type publishExecution struct {
	attempt       db.PublishAttempt
	fence         publishFence
	snapshot      PublishSnapshot
	idempotency   idempotency.Request
	idempotencyID *uuid.UUID
}

func NewPublishService(options PublishServiceOptions) (*PublishService, error) {
	if options.Pool == nil || options.Blobs == nil || options.Store == nil || options.Signer == nil || options.Audit == nil || options.Idempotency == nil {
		return nil, fmt.Errorf("publish service requires database, blob, storage, signer, audit, and idempotency dependencies")
	}
	if options.Clock == nil || options.IDs == nil {
		return nil, fmt.Errorf("publish service requires clock and ID generator")
	}
	if options.LeaseDuration <= 0 || options.IdempotencyTTL <= 0 {
		return nil, fmt.Errorf("publish service durations must be positive")
	}
	heartbeat := options.Heartbeat
	if heartbeat == 0 {
		heartbeat = options.LeaseDuration / 5
	}
	if heartbeat <= 0 || heartbeat >= options.LeaseDuration {
		return nil, fmt.Errorf("publish service heartbeat must be positive and shorter than lease")
	}
	keyID, fingerprint, err := signerIdentity(options.Signer)
	if err != nil {
		return nil, err
	}
	if keyID == "" || fingerprint == "" {
		return nil, fmt.Errorf("publish service signer identity is empty")
	}
	return &PublishService{
		pool:              options.Pool,
		blobs:             options.Blobs,
		store:             options.Store,
		signer:            options.Signer,
		audit:             options.Audit,
		idempotency:       options.Idempotency,
		clock:             options.Clock,
		ids:               options.IDs,
		leaseDuration:     options.LeaseDuration,
		heartbeatInterval: heartbeat,
		idempotencyTTL:    options.IdempotencyTTL,
		metrics:           options.Metrics,
		attempts:          newPublishAttemptStore(options.Pool),
	}, nil
}

func signerIdentity(signer signing.Signer) (string, string, error) {
	publicKey := signer.PublicKey()
	if len(publicKey) != 32 {
		return "", "", fmt.Errorf("signer public key has invalid size")
	}
	digest := sha256.Sum256(publicKey)
	fingerprint := hex.EncodeToString(digest[:])
	expected := "ed25519:" + fingerprint
	if signer.KeyID() != expected {
		return "", "", fmt.Errorf("signer key ID does not match public key fingerprint")
	}
	return signer.KeyID(), fingerprint, nil
}

func (s *PublishService) Publish(ctx context.Context, command PublishCommand) (PublishedRelease, error) {
	execution, replay, err := s.startPublish(ctx, command)
	if err != nil {
		return PublishedRelease{}, err
	}
	if replay != nil {
		return *replay, nil
	}
	published, err := s.executePublish(ctx, execution)
	if err == nil {
		return published, nil
	}
	if errors.Is(err, ErrLeaseLost) {
		return PublishedRelease{}, err
	}
	return PublishedRelease{}, s.handleExecutionFailure(ctx, execution, err, false)
}

func (s *PublishService) RecoverAttempt(ctx context.Context, attemptID uuid.UUID) (PublishedRelease, error) {
	if attemptID == uuid.Nil {
		return PublishedRelease{}, ErrInvalidRequest
	}
	owner := s.ids.New()
	if owner == uuid.Nil {
		return PublishedRelease{}, fmt.Errorf("generate recovery owner: nil UUID")
	}
	now := s.clock.Now().UTC()
	fence, err := s.attempts.takeExpired(ctx, attemptID, owner, now, now.Add(s.leaseDuration))
	if err != nil {
		return PublishedRelease{}, err
	}
	attempt, err := db.New(s.pool).GetPublishAttempt(ctx, attemptID)
	if err != nil {
		return PublishedRelease{}, fmt.Errorf("load publish recovery attempt: %w", err)
	}
	if attempt.LeaseOwner != owner || attempt.LeaseGeneration != fence.Generation || attempt.State != "active" {
		return PublishedRelease{}, ErrLeaseLost
	}
	execution := publishExecution{
		attempt: attempt,
		fence:   fence,
		idempotency: idempotency.Request{
			TokenID: attempt.ActorTokenID,
			Method:  "POST",
		},
		idempotencyID: attempt.IdempotencyRecordID,
	}
	snapshot, err := decodePublishSnapshot(attempt.Snapshot, attempt.SnapshotSha256)
	if err != nil {
		return PublishedRelease{}, s.handleExecutionFailure(ctx, execution, err, true)
	}
	execution.snapshot = snapshot
	execution.idempotency.CanonicalResource = "/api/v1/repositories/" + snapshot.Repository + "/packages/" + snapshot.Package + "/releases/" + snapshot.Version + "/publish"
	if attempt.KeyID != s.signer.KeyID() && !attempt.StorageCompleted {
		return PublishedRelease{}, s.handleExecutionFailure(
			ctx,
			execution,
			fmt.Errorf("%w: persisted key ID does not match active signer", ErrInvalidSnapshot),
			true,
		)
	}
	if attempt.StorageCompleted {
		published, err := s.finalizeRecovered(ctx, execution, snapshot)
		if err == nil {
			return published, nil
		}
		if errors.Is(err, ErrLeaseLost) {
			return PublishedRelease{}, err
		}
		return PublishedRelease{}, s.handleExecutionFailure(ctx, execution, err, true)
	}
	published, err := s.executePublish(ctx, execution)
	if err == nil {
		return published, nil
	}
	if errors.Is(err, ErrLeaseLost) {
		return PublishedRelease{}, err
	}
	return PublishedRelease{}, s.handleExecutionFailure(ctx, execution, err, true)
}

func (s *PublishService) GetManifest(
	ctx context.Context,
	actor auth.Actor,
	repositoryKey, packageName, version string,
) (SignedManifest, error) {
	if repositoryKey == "" || packageName == "" || version == "" {
		return SignedManifest{}, ErrInvalidRequest
	}
	queries := db.New(s.pool)
	_, packageRow, err := releasePackage(ctx, queries, actor, auth.ScopeArtifactRead, repositoryKey, packageName)
	if err != nil {
		return SignedManifest{}, err
	}
	releaseRow, err := queries.GetReleaseByVersion(ctx, db.GetReleaseByVersionParams{
		PackageID: packageRow.ID,
		Version:   version,
	})
	if err != nil {
		return SignedManifest{}, mapReleaseDatabaseError("get manifest release", err)
	}
	if releaseRow.State != "published" {
		return SignedManifest{}, ErrNotFound
	}
	row, err := queries.GetReleaseManifest(ctx, releaseRow.ID)
	if err != nil {
		return SignedManifest{}, mapReleaseDatabaseError("get release manifest", err)
	}
	manifest, err := readAllObject(ctx, s.store, row.ManifestObjectKey)
	if err != nil {
		return SignedManifest{}, fmt.Errorf("read release manifest: %w", err)
	}
	signature, err := readAllObject(ctx, s.store, row.SignatureObjectKey)
	if err != nil {
		return SignedManifest{}, fmt.Errorf("read release signature: %w", err)
	}
	if digestHex(manifest) != row.ManifestBlobSha256 || digestHex(signature) != row.SignatureBlobSha256 {
		return SignedManifest{}, fmt.Errorf("release manifest object checksum mismatch")
	}
	key, err := queries.GetSigningKey(ctx, row.KeyID)
	if err != nil {
		return SignedManifest{}, fmt.Errorf("get release manifest signing key: %w", err)
	}
	if len(key.PublicKey) != ed25519.PublicKeySize || !ed25519.Verify(ed25519.PublicKey(key.PublicKey), manifest, signature) {
		return SignedManifest{}, fmt.Errorf("release manifest signature verification failed")
	}
	return SignedManifest{
		Version:         releaseRow.Version,
		Manifest:        manifest,
		ManifestSHA256:  row.ManifestBlobSha256,
		KeyID:           row.KeyID,
		Signature:       signature,
		SignatureSHA256: row.SignatureBlobSha256,
	}, nil
}

func (s *PublishService) finalizeRecovered(ctx context.Context, execution publishExecution, snapshot PublishSnapshot) (PublishedRelease, error) {
	if execution.attempt.ManifestSha256 == nil || execution.attempt.SignatureSha256 == nil {
		return PublishedRelease{}, fmt.Errorf("%w: storage completion has no object digests", ErrInvalidSnapshot)
	}
	manifest, err := readAllObject(ctx, s.store, blob.ObjectKey(*execution.attempt.ManifestSha256))
	if err != nil {
		return PublishedRelease{}, fmt.Errorf("read recovered manifest: %w", err)
	}
	signature, err := readAllObject(ctx, s.store, blob.ObjectKey(*execution.attempt.SignatureSha256))
	if err != nil {
		return PublishedRelease{}, fmt.Errorf("read recovered signature: %w", err)
	}
	expectedManifest, err := BuildManifest(snapshot)
	if err != nil {
		return PublishedRelease{}, err
	}
	validSignature, err := s.verifySignature(ctx, execution.attempt.KeyID, manifest, signature)
	if err != nil {
		return PublishedRelease{}, err
	}
	if !bytes.Equal(manifest, expectedManifest) ||
		digestHex(manifest) != *execution.attempt.ManifestSha256 ||
		digestHex(signature) != *execution.attempt.SignatureSha256 ||
		!validSignature {
		return PublishedRelease{}, fmt.Errorf("%w: recovered signed objects do not match snapshot", ErrInvalidSnapshot)
	}
	return s.finalizePublish(ctx, execution, manifest, signature, *execution.attempt.ManifestSha256, *execution.attempt.SignatureSha256)
}

func (s *PublishService) handleExecutionFailure(ctx context.Context, execution publishExecution, cause error, recovering bool) error {
	if errors.Is(cause, context.Canceled) {
		return cause
	}
	code, permanent := publishFailureClass(cause)
	if permanent {
		if err := s.markPublishFailed(ctx, execution, code); err != nil {
			return err
		}
		return fmt.Errorf("%w: %v", ErrPublishFailed, cause)
	}
	if recovering && execution.attempt.RetryCount >= 10 {
		abortable, err := s.publishOutputsAbsent(ctx, execution)
		if err != nil {
			if markErr := s.markPublishFailed(ctx, execution, "publish-integrity-uncertain"); markErr != nil {
				return markErr
			}
			return fmt.Errorf("%w: final object absence could not be proven: %v", ErrPublishFailed, err)
		}
		if abortable {
			if err := s.abortPublishAttempt(ctx, execution, "publish-attempt-aborted"); err != nil {
				return err
			}
			return fmt.Errorf("%w: retry limit reached", ErrPublishAborted)
		}
		if err := s.markPublishFailed(ctx, execution, "publish-integrity-uncertain"); err != nil {
			return err
		}
		return fmt.Errorf("%w: retry limit reached with uncertain objects", ErrPublishFailed)
	}
	if err := s.recordTransientFailure(ctx, execution, code); err != nil {
		return err
	}
	return fmt.Errorf("%w: %v", ErrPublishPending, cause)
}

func publishFailureClass(err error) (string, bool) {
	switch {
	case errors.Is(err, ErrInvalidSnapshot), errors.Is(err, storage.ErrObjectConflict), errors.Is(err, blob.ErrSizeMismatch), errors.Is(err, signing.ErrInvalidKey):
		return "publish-integrity-failure", true
	case errors.Is(err, storage.ErrNotFound):
		return "publish-integrity-uncertain", true
	default:
		return "storage-transient", false
	}
}

func (s *PublishService) recordTransientFailure(ctx context.Context, execution publishExecution, code string) error {
	now := s.clock.Now().UTC()
	nextRetry := now.Add(publishRetryBackoff(execution.attempt.RetryCount))
	err := s.attempts.finalize(ctx, execution.fence, now, func(ctx context.Context, tx pgx.Tx) error {
		failureCode := code
		rows, err := db.New(tx).RecordPublishFailure(ctx, db.RecordPublishFailureParams{
			ID:              execution.fence.AttemptID,
			LeaseOwner:      execution.fence.Owner,
			LeaseGeneration: execution.fence.Generation,
			FailureCode:     &failureCode,
			NextRetryAt:     pgtype.Timestamptz{Time: nextRetry, Valid: true},
			UpdatedAt:       pgtype.Timestamptz{Time: now, Valid: true},
		})
		if err != nil {
			return fmt.Errorf("record publish failure: %w", err)
		}
		if rows != 1 {
			return ErrLeaseLost
		}
		return nil
	})
	return err
}

func publishRetryBackoff(retryCount int32) time.Duration {
	backoff := 5 * time.Second
	for index := int32(0); index < retryCount; index++ {
		if backoff >= 5*time.Minute {
			return 5 * time.Minute
		}
		backoff *= 2
	}
	if backoff > 5*time.Minute {
		return 5 * time.Minute
	}
	return backoff
}

func (s *PublishService) markPublishFailed(ctx context.Context, execution publishExecution, code string) error {
	now := s.clock.Now().UTC()
	err := s.attempts.finalize(ctx, execution.fence, now, func(ctx context.Context, tx pgx.Tx) error {
		failureCode := code
		attemptID := execution.fence.AttemptID
		rows, err := db.New(tx).MarkPublishAttemptFailed(ctx, db.MarkPublishAttemptFailedParams{
			CurrentAttemptID: &attemptID,
			LeaseOwner:       execution.fence.Owner,
			LeaseGeneration:  execution.fence.Generation,
			FailureCode:      &failureCode,
			UpdatedAt:        pgtype.Timestamptz{Time: now, Valid: true},
		})
		if err != nil {
			return fmt.Errorf("mark publish attempt failed: %w", err)
		}
		if rows != 1 {
			return ErrLeaseLost
		}
		contextRow, err := db.New(tx).GetPublishReleaseContext(ctx, execution.attempt.ReleaseID)
		if err != nil {
			return fmt.Errorf("load failed publish context: %w", err)
		}
		if _, err := s.audit.Record(ctx, tx, audit.Event{
			ActorTokenID: execution.attempt.ActorTokenID,
			RepositoryID: contextRow.RepositoryID,
			Action:       "release.publish",
			ResourceType: "release",
			ResourceID:   execution.attempt.ReleaseID.String(),
			Outcome:      audit.OutcomeFailed,
			Code:         code,
			RequestID:    execution.attempt.RequestID,
			Details:      map[string]any{"attemptId": execution.attempt.ID.String()},
		}); err != nil {
			return err
		}
		body, err := json.Marshal(struct {
			Code string `json:"code"`
		}{Code: code})
		if err != nil {
			return err
		}
		return s.idempotency.CompleteInTx(ctx, tx, execution.idempotencyID, execution.idempotency, idempotency.Response{
			Status: 500,
			Body:   body,
		})
	})
	return err
}

func (s *PublishService) abortPublishAttempt(ctx context.Context, execution publishExecution, code string) error {
	now := s.clock.Now().UTC()
	err := s.attempts.finalize(ctx, execution.fence, now, func(ctx context.Context, tx pgx.Tx) error {
		failureCode := code
		attemptID := execution.fence.AttemptID
		rows, err := db.New(tx).AbortPublishAttemptToDraft(ctx, db.AbortPublishAttemptToDraftParams{
			CurrentAttemptID: &attemptID,
			LeaseOwner:       execution.fence.Owner,
			LeaseGeneration:  execution.fence.Generation,
			FailureCode:      &failureCode,
			UpdatedAt:        pgtype.Timestamptz{Time: now, Valid: true},
		})
		if err != nil {
			return fmt.Errorf("abort publish attempt: %w", err)
		}
		if rows != 1 {
			return ErrLeaseLost
		}
		contextRow, err := db.New(tx).GetPublishReleaseContext(ctx, execution.attempt.ReleaseID)
		if err != nil {
			return fmt.Errorf("load aborted publish context: %w", err)
		}
		if _, err := s.audit.Record(ctx, tx, audit.Event{
			ActorTokenID: execution.attempt.ActorTokenID,
			RepositoryID: contextRow.RepositoryID,
			Action:       "release.publish",
			ResourceType: "release",
			ResourceID:   execution.attempt.ReleaseID.String(),
			Outcome:      audit.OutcomeFailed,
			Code:         code,
			RequestID:    execution.attempt.RequestID,
			Details:      map[string]any{"attemptId": execution.attempt.ID.String()},
		}); err != nil {
			return err
		}
		body, err := json.Marshal(struct {
			Code string `json:"code"`
		}{Code: code})
		if err != nil {
			return err
		}
		return s.idempotency.CompleteInTx(ctx, tx, execution.idempotencyID, execution.idempotency, idempotency.Response{
			Status: 503,
			Body:   body,
		})
	})
	return err
}

func (s *PublishService) publishOutputsAbsent(ctx context.Context, execution publishExecution) (bool, error) {
	manifest, err := BuildManifest(execution.snapshot)
	if err != nil {
		return false, err
	}
	signature, err := s.signer.Sign(ctx, manifest)
	if err != nil {
		return false, err
	}
	hashes := []string{digestHex(manifest), digestHex(signature)}
	for _, hash := range hashes {
		_, err := s.store.Stat(ctx, blob.ObjectKey(hash))
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return false, err
		}
		return false, nil
	}
	return true, nil
}

func (s *PublishService) verifySignature(ctx context.Context, keyID string, manifest, signature []byte) (bool, error) {
	key, err := db.New(s.pool).GetSigningKey(ctx, keyID)
	if err != nil {
		return false, fmt.Errorf("get publish signing key: %w", err)
	}
	if len(key.PublicKey) != ed25519.PublicKeySize {
		return false, fmt.Errorf("publish signing key has invalid size")
	}
	return ed25519.Verify(ed25519.PublicKey(key.PublicKey), manifest, signature), nil
}

func digestHex(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

func (s *PublishService) startPublish(ctx context.Context, command PublishCommand) (publishExecution, *PublishedRelease, error) {
	if err := validateMutation(command.Mutation); err != nil {
		return publishExecution{}, nil, err
	}
	if command.RepositoryKey == "" || command.PackageName == "" || command.Version == "" {
		return publishExecution{}, nil, ErrInvalidRequest
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return publishExecution{}, nil, fmt.Errorf("begin publish attempt: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	idempotencyRequest := mutationIdempotencyRequest(command.Mutation, s.idempotencyTTL)
	idempotencyBegin, err := s.idempotency.BeginInTx(ctx, tx, idempotencyRequest)
	if err != nil {
		return publishExecution{}, nil, err
	}
	completeValidationFailure := func(cause error) (publishExecution, *PublishedRelease, error) {
		status, code, deterministic := deterministicPublishFailure(cause)
		if !deterministic {
			return publishExecution{}, nil, cause
		}
		body, err := json.Marshal(struct {
			Code string `json:"code"`
		}{Code: code})
		if err != nil {
			return publishExecution{}, nil, err
		}
		if err := s.idempotency.CompleteInTx(ctx, tx, idempotencyBegin.RecordID, idempotencyRequest, idempotency.Response{
			Status: status,
			Body:   body,
		}); err != nil {
			return publishExecution{}, nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return publishExecution{}, nil, fmt.Errorf("commit publish validation failure: %w", err)
		}
		return publishExecution{}, nil, cause
	}
	if idempotencyBegin.Replay != nil {
		if err := tx.Commit(ctx); err != nil {
			return publishExecution{}, nil, fmt.Errorf("commit publish replay: %w", err)
		}
		if idempotencyBegin.Replay.Status >= 400 {
			return publishExecution{}, nil, replayPublishFailure(idempotencyBegin.Replay.Status, idempotencyBegin.Replay.Body)
		}
		var replay PublishedRelease
		if err := json.Unmarshal(idempotencyBegin.Replay.Body, &replay); err != nil {
			return publishExecution{}, nil, fmt.Errorf("decode published release replay: %w", err)
		}
		replay.Replayed = true
		return publishExecution{}, &replay, nil
	}

	queries := db.New(tx)
	repository, packageRow, err := releasePackage(
		ctx,
		queries,
		command.Mutation.Actor,
		auth.ScopeReleasePublish,
		command.RepositoryKey,
		command.PackageName,
	)
	if err != nil {
		return completeValidationFailure(err)
	}
	releaseRow, err := queries.GetReleaseForUpdate(ctx, db.GetReleaseForUpdateParams{
		PackageID: packageRow.ID,
		Version:   command.Version,
	})
	if err != nil {
		return completeValidationFailure(mapReleaseDatabaseError("lock release for publishing", err))
	}
	if releaseRow.State != "draft" {
		return completeValidationFailure(ErrConflict)
	}
	artifactRows, err := queries.ListReleaseArtifacts(ctx, releaseRow.ID)
	if err != nil {
		return publishExecution{}, nil, fmt.Errorf("list publish artifacts: %w", err)
	}
	if len(artifactRows) == 0 {
		return completeValidationFailure(ErrNoArtifacts)
	}

	now := s.clock.Now().UTC()
	snapshot := PublishSnapshot{
		Repository:  repository.Key,
		Package:     packageRow.Name,
		Version:     releaseRow.Version,
		PublishedAt: now,
		Artifacts:   make([]SnapshotArtifact, 0, len(artifactRows)),
	}
	for _, row := range artifactRows {
		if row.RepositoryID != repository.ID {
			return completeValidationFailure(ErrUnprocessable)
		}
		if row.BlobState != string(blob.StateReady) {
			return completeValidationFailure(ErrPublishNotReady)
		}
		snapshot.Artifacts = append(snapshot.Artifacts, SnapshotArtifact{
			Path:      row.LogicalPath,
			Filename:  row.Filename,
			OS:        row.Os,
			Arch:      row.Arch,
			Variant:   row.Variant,
			Role:      row.Role,
			MediaType: row.MediaType,
			SHA256:    row.BlobSha256,
			Size:      row.Size,
		})
	}
	if _, err := BuildManifest(snapshot); err != nil {
		return completeValidationFailure(fmt.Errorf("validate publish snapshot: %w", err))
	}
	snapshotBytes, err := canonicalSnapshot(snapshot)
	if err != nil {
		return publishExecution{}, nil, err
	}
	snapshotDigest := sha256.Sum256(snapshotBytes)
	keyID, fingerprint, err := signerIdentity(s.signer)
	if err != nil {
		return publishExecution{}, nil, err
	}
	if _, err := queries.UpsertSigningKey(ctx, db.UpsertSigningKeyParams{
		KeyID:       keyID,
		PublicKey:   append([]byte(nil), s.signer.PublicKey()...),
		Fingerprint: fingerprint,
		Active:      true,
	}); err != nil {
		return publishExecution{}, nil, fmt.Errorf("persist signing key: %w", err)
	}

	attemptID := s.ids.New()
	owner := s.ids.New()
	if attemptID == uuid.Nil || owner == uuid.Nil {
		return publishExecution{}, nil, fmt.Errorf("generate publish identifiers: nil UUID")
	}
	attempt, err := queries.CreatePublishAttempt(ctx, db.CreatePublishAttemptParams{
		ID:                  attemptID,
		ReleaseID:           releaseRow.ID,
		IdempotencyRecordID: idempotencyBegin.RecordID,
		ActorTokenID:        command.Mutation.Actor.TokenID,
		RequestID:           command.Mutation.RequestID,
		PublishedAt:         pgtype.Timestamptz{Time: now, Valid: true},
		Snapshot:            snapshotBytes,
		SnapshotSha256:      hex.EncodeToString(snapshotDigest[:]),
		KeyID:               keyID,
		LeaseOwner:          owner,
		LeaseExpiresAt:      pgtype.Timestamptz{Time: now.Add(s.leaseDuration), Valid: true},
	})
	if err != nil {
		return publishExecution{}, nil, mapReleaseDatabaseError("create publish attempt", err)
	}
	rows, err := queries.SetReleasePublishing(ctx, db.SetReleasePublishingParams{
		ID:               releaseRow.ID,
		CurrentAttemptID: &attemptID,
		UpdatedAt:        pgtype.Timestamptz{Time: now, Valid: true},
	})
	if err != nil {
		return publishExecution{}, nil, fmt.Errorf("set release publishing: %w", err)
	}
	if rows != 1 {
		return publishExecution{}, nil, ErrConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return publishExecution{}, nil, fmt.Errorf("commit publish attempt: %w", err)
	}
	persistedSnapshot, err := decodePublishSnapshot(attempt.Snapshot, attempt.SnapshotSha256)
	if err != nil {
		return publishExecution{}, nil, err
	}
	return publishExecution{
		attempt:       attempt,
		fence:         publishFence{AttemptID: attempt.ID, Owner: attempt.LeaseOwner, Generation: attempt.LeaseGeneration},
		snapshot:      persistedSnapshot,
		idempotency:   idempotencyRequest,
		idempotencyID: idempotencyBegin.RecordID,
	}, nil, nil
}

func replayPublishFailure(status int, body []byte) error {
	var stored struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &stored); err != nil {
		return fmt.Errorf("decode publish failure replay: %w", err)
	}
	switch {
	case status == 400 || stored.Code == "invalid-request" || stored.Code == "invalid-snapshot":
		return fmt.Errorf("%w: %s", ErrInvalidRequest, stored.Code)
	case status == 403 || stored.Code == "forbidden":
		return fmt.Errorf("%w: %s", auth.ErrForbidden, stored.Code)
	case status == 404 || stored.Code == "not-found":
		return fmt.Errorf("%w: %s", ErrNotFound, stored.Code)
	case stored.Code == "release-not-ready":
		return fmt.Errorf("%w: %s", ErrPublishNotReady, stored.Code)
	case status == 409 || stored.Code == "conflict":
		return fmt.Errorf("%w: %s", ErrConflict, stored.Code)
	case stored.Code == "release-has-no-artifacts":
		return fmt.Errorf("%w: %s", ErrNoArtifacts, stored.Code)
	case status == 422 || stored.Code == "cross-repository-artifact":
		return fmt.Errorf("%w: %s", ErrUnprocessable, stored.Code)
	case status == 503 || stored.Code == "publish-attempt-aborted":
		return fmt.Errorf("%w: %s", ErrPublishAborted, stored.Code)
	case status >= 500 || stored.Code == "publish-integrity-failure" || stored.Code == "publish-integrity-uncertain":
		return fmt.Errorf("%w: %s", ErrPublishFailed, stored.Code)
	default:
		return fmt.Errorf("publish replay failed with status %d and code %s", status, stored.Code)
	}
}

func deterministicPublishFailure(err error) (int, string, bool) {
	switch {
	case errors.Is(err, auth.ErrForbidden):
		return 403, "forbidden", true
	case errors.Is(err, ErrNotFound):
		return 404, "not-found", true
	case errors.Is(err, ErrPublishNotReady):
		return 409, "release-not-ready", true
	case errors.Is(err, ErrConflict):
		return 409, "conflict", true
	case errors.Is(err, ErrNoArtifacts):
		return 422, "release-has-no-artifacts", true
	case errors.Is(err, ErrUnprocessable):
		return 422, "cross-repository-artifact", true
	case errors.Is(err, ErrInvalidSnapshot):
		return 422, "invalid-snapshot", true
	case errors.Is(err, ErrInvalidRequest):
		return 400, "invalid-request", true
	default:
		return 0, "", false
	}
}

func (s *PublishService) executePublish(ctx context.Context, execution publishExecution) (PublishedRelease, error) {
	manifest, err := BuildManifest(execution.snapshot)
	if err != nil {
		return PublishedRelease{}, fmt.Errorf("build persisted publish manifest: %w", err)
	}
	if err := s.renew(ctx, execution.fence); err != nil {
		return PublishedRelease{}, err
	}
	var signature []byte
	var manifestSHA, signatureSHA string
	err = s.withLeaseHeartbeat(ctx, execution.fence, func(operationContext context.Context) error {
		var err error
		signature, err = s.signer.Sign(operationContext, manifest)
		if err != nil {
			if s.metrics != nil {
				s.metrics.ObserveSigningFailure(signingFailureCode(err))
			}
			return fmt.Errorf("sign release manifest: %w", err)
		}
		manifestSHA, err = s.persistPublishBlob(operationContext, execution.fence, "manifest", manifest)
		if err != nil {
			return fmt.Errorf("persist release manifest: %w", err)
		}
		signatureSHA, err = s.persistPublishBlob(operationContext, execution.fence, "signature", signature)
		if err != nil {
			return fmt.Errorf("persist release signature: %w", err)
		}
		return nil
	})
	if err != nil {
		return PublishedRelease{}, err
	}
	if err := s.renew(ctx, execution.fence); err != nil {
		return PublishedRelease{}, err
	}
	if err := s.recordPublishStorage(ctx, execution.fence, manifestSHA, signatureSHA); err != nil {
		return PublishedRelease{}, err
	}
	return s.finalizePublish(ctx, execution, manifest, signature, manifestSHA, signatureSHA)
}

func signingFailureCode(err error) string {
	if errors.Is(err, signing.ErrInvalidKey) {
		return "invalid_key"
	}
	return "sign_failed"
}

func (s *PublishService) recordPublishStorage(ctx context.Context, fence publishFence, manifestSHA, signatureSHA string) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin publish storage record: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := s.blobs.Reference(ctx, tx, manifestSHA); err != nil {
		return fmt.Errorf("reference publish manifest blob: %w", err)
	}
	if _, err := s.blobs.Reference(ctx, tx, signatureSHA); err != nil {
		return fmt.Errorf("reference publish signature blob: %w", err)
	}
	rows, err := db.New(tx).RecordPublishStorage(ctx, db.RecordPublishStorageParams{
		ID:              fence.AttemptID,
		LeaseOwner:      fence.Owner,
		LeaseGeneration: fence.Generation,
		ManifestSha256:  &manifestSHA,
		SignatureSha256: &signatureSHA,
		UpdatedAt:       pgtype.Timestamptz{Time: s.clock.Now().UTC(), Valid: true},
	})
	if err != nil {
		return fmt.Errorf("record publish storage: %w", err)
	}
	if rows != 1 {
		return ErrLeaseLost
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit publish storage record: %w", err)
	}
	return nil
}

func (s *PublishService) withLeaseHeartbeat(
	ctx context.Context,
	fence publishFence,
	operation func(context.Context) error,
) error {
	operationContext, cancel := context.WithCancel(ctx)
	defer cancel()
	heartbeatResult := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(s.heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-operationContext.Done():
				heartbeatResult <- nil
				return
			case <-ticker.C:
				if err := s.renew(operationContext, fence); err != nil {
					heartbeatResult <- publishHeartbeatError(operationContext, err)
					cancel()
					return
				}
				if operationContext.Err() != nil {
					heartbeatResult <- nil
					return
				}
			}
		}
	}()
	operationErr := operation(operationContext)
	cancel()
	heartbeatErr := <-heartbeatResult
	if heartbeatErr != nil {
		return heartbeatErr
	}
	return operationErr
}

func publishHeartbeatError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func canonicalSnapshot(snapshot PublishSnapshot) ([]byte, error) {
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return nil, fmt.Errorf("encode publish snapshot: %w", err)
	}
	canonical, err := jcs.Transform(encoded)
	if err != nil {
		return nil, fmt.Errorf("canonicalize publish snapshot: %w", err)
	}
	return canonical, nil
}

func decodePublishSnapshot(encoded []byte, expectedSHA string) (PublishSnapshot, error) {
	canonical, err := jcs.Transform(encoded)
	if err != nil {
		return PublishSnapshot{}, fmt.Errorf("canonicalize persisted publish snapshot: %w", err)
	}
	digest := sha256.Sum256(canonical)
	if !strings.EqualFold(expectedSHA, hex.EncodeToString(digest[:])) {
		return PublishSnapshot{}, fmt.Errorf("%w: persisted snapshot checksum mismatch", ErrInvalidSnapshot)
	}
	var snapshot PublishSnapshot
	if err := json.Unmarshal(canonical, &snapshot); err != nil {
		return PublishSnapshot{}, fmt.Errorf("decode persisted publish snapshot: %w", err)
	}
	if _, err := BuildManifest(snapshot); err != nil {
		return PublishSnapshot{}, err
	}
	return snapshot, nil
}

func (s *PublishService) renew(ctx context.Context, fence publishFence) error {
	now := s.clock.Now().UTC()
	rows, err := db.New(s.pool).RenewPublishLease(ctx, db.RenewPublishLeaseParams{
		ID:              fence.AttemptID,
		LeaseOwner:      fence.Owner,
		LeaseGeneration: fence.Generation,
		LeaseExpiresAt:  pgtype.Timestamptz{Time: now.Add(s.leaseDuration), Valid: true},
	})
	if err != nil {
		return fmt.Errorf("renew publish lease: %w", err)
	}
	if rows != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (s *PublishService) persistPublishBlob(ctx context.Context, fence publishFence, kind string, content []byte) (string, error) {
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	claim, err := s.blobs.Claim(ctx, blob.ClaimRequest{SHA256: sha, Size: int64(len(content)), Owner: fence.Owner})
	if err != nil {
		return "", err
	}
	switch claim.Decision {
	case blob.DecisionReady:
		info, err := s.store.Stat(ctx, claim.ObjectKey)
		if err != nil {
			return "", err
		}
		if info.Size != int64(len(content)) {
			return "", storage.ErrObjectConflict
		}
	case blob.DecisionOwned:
		stagingKey := "staging/publish/" + fence.AttemptID.String() + "/" + kind
		if err := s.store.PutStaging(ctx, stagingKey, bytes.NewReader(content), int64(len(content))); err != nil {
			return "", err
		}
		if err := s.store.Promote(ctx, stagingKey, claim.ObjectKey, int64(len(content))); err != nil {
			return "", err
		}
		if _, err := s.blobs.MarkReady(ctx, blob.Fence{SHA256: sha, Owner: fence.Owner, Generation: claim.Generation}); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("unknown publish blob decision %q", claim.Decision)
	}
	return sha, nil
}

func (s *PublishService) finalizePublish(
	ctx context.Context,
	execution publishExecution,
	manifest, signature []byte,
	manifestSHA, signatureSHA string,
) (PublishedRelease, error) {
	var published PublishedRelease
	err := s.attempts.finalize(ctx, execution.fence, s.clock.Now().UTC(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := s.blobs.Reference(ctx, tx, manifestSHA); err != nil {
			return fmt.Errorf("reference manifest blob: %w", err)
		}
		if _, err := s.blobs.Reference(ctx, tx, signatureSHA); err != nil {
			return fmt.Errorf("reference signature blob: %w", err)
		}
		queries := db.New(tx)
		if _, err := queries.InsertReleaseManifest(ctx, db.InsertReleaseManifestParams{
			ReleaseID:           execution.attempt.ReleaseID,
			AttemptID:           execution.attempt.ID,
			ManifestBlobSha256:  manifestSHA,
			SignatureBlobSha256: signatureSHA,
			KeyID:               execution.attempt.KeyID,
		}); err != nil {
			return fmt.Errorf("insert release manifest: %w", err)
		}
		attemptID := execution.fence.AttemptID
		rows, err := queries.FinalizePublishedRelease(ctx, db.FinalizePublishedReleaseParams{
			CurrentAttemptID: &attemptID,
			LeaseOwner:       execution.fence.Owner,
			LeaseGeneration:  execution.fence.Generation,
			UpdatedAt:        pgtype.Timestamptz{Time: s.clock.Now().UTC(), Valid: true},
		})
		if err != nil {
			return fmt.Errorf("finalize published release: %w", err)
		}
		if rows != 1 {
			return ErrLeaseLost
		}

		repository, err := queries.GetRepositoryByKey(ctx, execution.snapshot.Repository)
		if err != nil {
			return fmt.Errorf("load published repository: %w", err)
		}
		packageRow, err := queries.GetPackageByName(ctx, db.GetPackageByNameParams{
			Key:  execution.snapshot.Repository,
			Name: execution.snapshot.Package,
		})
		if err != nil {
			return fmt.Errorf("load published package: %w", err)
		}
		releaseRow, err := queries.GetReleaseByVersion(ctx, db.GetReleaseByVersionParams{
			PackageID: packageRow.ID,
			Version:   execution.snapshot.Version,
		})
		if err != nil {
			return fmt.Errorf("load published release: %w", err)
		}
		artifacts, err := releaseArtifacts(ctx, queries, releaseRow.ID, repository.Key)
		if err != nil {
			return err
		}
		published = PublishedRelease{
			Release:         releaseFromRow(releaseRow, repository, packageRow, artifacts),
			Manifest:        append([]byte(nil), manifest...),
			Signature:       append([]byte(nil), signature...),
			ManifestSHA256:  manifestSHA,
			SignatureSHA256: signatureSHA,
			KeyID:           execution.attempt.KeyID,
			AttemptID:       execution.attempt.ID,
		}
		if _, err := s.audit.Record(ctx, tx, audit.Event{
			ActorTokenID: execution.attempt.ActorTokenID,
			RepositoryID: repository.ID,
			Action:       "release.publish",
			ResourceType: "release",
			ResourceID:   releaseRow.ID.String(),
			Outcome:      audit.OutcomeSuccess,
			RequestID:    execution.attempt.RequestID,
			Details: map[string]any{
				"attemptId":      execution.attempt.ID.String(),
				"manifestSha256": manifestSHA,
				"package":        packageRow.Name,
				"version":        releaseRow.Version,
			},
		}); err != nil {
			return err
		}
		body, err := json.Marshal(published)
		if err != nil {
			return fmt.Errorf("encode published release response: %w", err)
		}
		if err := s.idempotency.CompleteInTx(ctx, tx, execution.idempotencyID, execution.idempotency, idempotency.Response{
			Status: 200,
			Body:   body,
		}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return PublishedRelease{}, err
	}
	return published, nil
}

func readAllObject(ctx context.Context, store storage.Store, key string) ([]byte, error) {
	object, err := store.Open(ctx, key, "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = object.Body.Close() }()
	content, err := io.ReadAll(object.Body)
	if err != nil {
		return nil, fmt.Errorf("read stored object %q: %w", key, err)
	}
	return content, nil
}

type publishFence struct {
	AttemptID  uuid.UUID
	Owner      uuid.UUID
	Generation int64
}

type publishAttemptStore struct {
	pool *pgxpool.Pool
}

func newPublishAttemptStore(pool *pgxpool.Pool) *publishAttemptStore {
	return &publishAttemptStore{pool: pool}
}

func (s *publishAttemptStore) takeExpired(
	ctx context.Context,
	attemptID, owner uuid.UUID,
	now, leaseExpiresAt time.Time,
) (publishFence, error) {
	if s == nil || s.pool == nil || attemptID == uuid.Nil || owner == uuid.Nil {
		return publishFence{}, fmt.Errorf("take publish lease: invalid arguments")
	}
	row, err := db.New(s.pool).TakePublishAttemptLease(ctx, db.TakePublishAttemptLeaseParams{
		ID:             attemptID,
		LeaseOwner:     owner,
		UpdatedAt:      pgtype.Timestamptz{Time: now.UTC(), Valid: true},
		LeaseExpiresAt: pgtype.Timestamptz{Time: leaseExpiresAt.UTC(), Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return publishFence{}, ErrLeaseLost
	}
	if err != nil {
		return publishFence{}, fmt.Errorf("take publish lease: %w", err)
	}
	return publishFence{AttemptID: row.ID, Owner: row.LeaseOwner, Generation: row.LeaseGeneration}, nil
}

func (s *publishAttemptStore) finalize(
	ctx context.Context,
	fence publishFence,
	now time.Time,
	callback func(context.Context, pgx.Tx) error,
) error {
	if s == nil || s.pool == nil || fence.AttemptID == uuid.Nil || fence.Owner == uuid.Nil || callback == nil {
		return fmt.Errorf("finalize publish attempt: invalid arguments")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin publish finalization: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := db.New(tx).FencePublishAttempt(ctx, db.FencePublishAttemptParams{
		ID:              fence.AttemptID,
		LeaseOwner:      fence.Owner,
		LeaseGeneration: fence.Generation,
		UpdatedAt:       pgtype.Timestamptz{Time: now.UTC(), Valid: true},
	})
	if err != nil {
		return fmt.Errorf("fence publish finalization: %w", err)
	}
	if rows != 1 {
		return ErrLeaseLost
	}
	if err := callback(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit publish finalization: %w", err)
	}
	return nil
}
