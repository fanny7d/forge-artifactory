package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"superfan.myasustor.com/fanchao/artifact-repository/internal/audit"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/auth"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/blob"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/clock"
	db "superfan.myasustor.com/fanchao/artifact-repository/internal/database/sqlc"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/id"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/storage"
)

var (
	ErrNotFound                  = errors.New("artifact not found")
	ErrConflict                  = errors.New("artifact path already exists")
	ErrInvalidRequest            = errors.New("invalid artifact request")
	ErrTooLarge                  = errors.New("artifact exceeds upload limit")
	ErrLengthMismatch            = errors.New("artifact body does not match Content-Length")
	ErrChecksumMismatch          = errors.New("artifact checksum mismatch")
	ErrUploadIdle                = errors.New("artifact upload was idle for too long")
	ErrUploadLeaseLost           = errors.New("upload session lease lost")
	ErrPublicEndpointUnavailable = storage.ErrPublicEndpointUnavailable
)

type Options struct {
	Pool              *pgxpool.Pool
	Blobs             *blob.Service
	Store             storage.Store
	Audit             *audit.Service
	Clock             clock.Clock
	IDs               id.Generator
	MaxUploadBytes    int64
	UploadIdleTimeout time.Duration
	UploadLease       time.Duration
	UploadHeartbeat   time.Duration
	UploadMaxDuration time.Duration
	PresignTTL        time.Duration
	Metrics           BlobDedupMetrics
}

type BlobDedupMetrics interface {
	ObserveBlobDedup(bool)
}

type Service struct {
	pool              *pgxpool.Pool
	blobs             *blob.Service
	store             storage.Store
	audit             *audit.Service
	clock             clock.Clock
	ids               id.Generator
	maxUploadBytes    int64
	uploadIdleTimeout time.Duration
	uploadLease       time.Duration
	uploadHeartbeat   time.Duration
	uploadMaxDuration time.Duration
	presignTTL        time.Duration
	metrics           BlobDedupMetrics
}

type UploadRequest struct {
	Actor          auth.Actor
	RequestID      string
	RepositoryKey  string
	RawPath        string
	Body           io.Reader
	ContentLength  int64
	MediaType      string
	Properties     map[string]any
	ExpectedSHA256 string
}

type ChecksumDeployRequest struct {
	Actor         auth.Actor
	RequestID     string
	RepositoryKey string
	RawPath       string
	SHA256        string
	MediaType     string
	Properties    map[string]any
}

type Metadata struct {
	ID            uuid.UUID
	RepositoryID  uuid.UUID
	RepositoryKey string
	Path          string
	Filename      string
	MediaType     string
	Size          int64
	SHA256        string
	Properties    map[string]any
	CreatedBy     uuid.UUID
	CreatedAt     time.Time
}

type OpenRequest struct {
	Actor         auth.Actor
	RepositoryKey string
	RawPath       string
	Redirect      *bool
	Range         string
}

type OpenResult struct {
	Metadata    Metadata
	RedirectURL string
	Object      storage.Object
}

func NewService(options Options) (*Service, error) {
	if options.Pool == nil || options.Blobs == nil || options.Store == nil || options.Audit == nil {
		return nil, fmt.Errorf("artifact service requires database, blob, storage, and audit dependencies")
	}
	if options.Clock == nil || options.IDs == nil {
		return nil, fmt.Errorf("artifact service requires clock and ID generator")
	}
	if options.MaxUploadBytes <= 0 || options.UploadIdleTimeout <= 0 || options.UploadLease <= 0 ||
		options.UploadHeartbeat <= 0 || options.UploadMaxDuration <= 0 || options.PresignTTL <= 0 {
		return nil, fmt.Errorf("artifact service durations and size limits must be positive")
	}
	if options.UploadHeartbeat >= options.UploadLease || options.UploadIdleTimeout >= options.UploadMaxDuration {
		return nil, fmt.Errorf("artifact service upload timing is invalid")
	}
	return &Service{
		pool:              options.Pool,
		blobs:             options.Blobs,
		store:             options.Store,
		audit:             options.Audit,
		clock:             options.Clock,
		ids:               options.IDs,
		maxUploadBytes:    options.MaxUploadBytes,
		uploadIdleTimeout: options.UploadIdleTimeout,
		uploadLease:       options.UploadLease,
		uploadHeartbeat:   options.UploadHeartbeat,
		uploadMaxDuration: options.UploadMaxDuration,
		presignTTL:        options.PresignTTL,
		metrics:           options.Metrics,
	}, nil
}

func (s *Service) Upload(ctx context.Context, request UploadRequest) (Metadata, error) {
	path, err := NormalizePath(request.RawPath)
	if err != nil {
		return Metadata{}, ErrInvalidRequest
	}
	if request.Body == nil || request.RequestID == "" || request.ContentLength < 0 {
		return Metadata{}, ErrInvalidRequest
	}
	if request.ContentLength > s.maxUploadBytes {
		return Metadata{}, ErrTooLarge
	}
	if request.ExpectedSHA256 != "" && !validSHA256(request.ExpectedSHA256) {
		return Metadata{}, ErrInvalidRequest
	}
	mediaType := request.MediaType
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	properties, err := encodeProperties(request.Properties)
	if err != nil {
		return Metadata{}, err
	}

	queries := db.New(s.pool)
	repository, err := queries.GetRepositoryByKey(ctx, request.RepositoryKey)
	if err != nil {
		return Metadata{}, mapArtifactDatabaseError("get upload repository", err)
	}
	if err := authorizeRepository(request.Actor, auth.ScopeArtifactWrite, repository.ID); err != nil {
		return Metadata{}, err
	}
	if _, err := queries.GetArtifactByPath(ctx, db.GetArtifactByPathParams{RepositoryID: repository.ID, LogicalPath: path}); err == nil {
		return Metadata{}, ErrConflict
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return Metadata{}, fmt.Errorf("check immutable artifact path: %w", err)
	}

	now := s.clock.Now().UTC()
	sessionID := s.ids.New()
	owner := s.ids.New()
	stagingKey := "staging/uploads/" + sessionID.String()
	if _, err := queries.CreateUploadSession(ctx, db.CreateUploadSessionParams{
		ID:              sessionID,
		RepositoryID:    repository.ID,
		LogicalPath:     path,
		StagingKey:      stagingKey,
		LeaseOwner:      owner,
		LeaseExpiresAt:  pgtype.Timestamptz{Time: now.Add(s.uploadLease), Valid: true},
		HardDeadline:    pgtype.Timestamptz{Time: now.Add(s.uploadMaxDuration), Valid: true},
		LastHeartbeatAt: pgtype.Timestamptz{Time: now, Valid: true},
		CreatedBy:       request.Actor.TokenID,
	}); err != nil {
		return Metadata{}, mapArtifactDatabaseError("create upload session", err)
	}

	uploadCtx, cancel := context.WithTimeout(ctx, s.uploadMaxDuration)
	defer cancel()
	digest := sha256.New()
	limited := &io.LimitedReader{R: request.Body, N: request.ContentLength + 1}
	counter := &countingReader{reader: io.TeeReader(limited, digest)}
	heartbeat := &heartbeatReader{
		reader:   &idleTimeoutReader{reader: counter, closer: readerCloser(request.Body), timeout: s.uploadIdleTimeout},
		next:     now.Add(s.uploadHeartbeat),
		interval: s.uploadHeartbeat,
		now:      s.clock.Now,
		beat: func() error {
			return s.heartbeat(uploadCtx, sessionID, owner, 0)
		},
	}
	putErr := s.store.PutStaging(uploadCtx, stagingKey, heartbeat, request.ContentLength)
	if putErr != nil {
		s.failUploadSession(ctx, sessionID, owner, 0, stagingKey)
		if errors.Is(putErr, ErrUploadIdle) {
			return Metadata{}, ErrUploadIdle
		}
		if counter.count != request.ContentLength {
			return Metadata{}, ErrLengthMismatch
		}
		return Metadata{}, fmt.Errorf("store staging artifact: %w", putErr)
	}
	if counter.count != request.ContentLength {
		s.failUploadSession(ctx, sessionID, owner, 0, stagingKey)
		return Metadata{}, ErrLengthMismatch
	}
	var overflow [1]byte
	read, readErr := heartbeat.Read(overflow[:])
	if read > 0 {
		s.failUploadSession(ctx, sessionID, owner, 0, stagingKey)
		return Metadata{}, ErrLengthMismatch
	}
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		s.failUploadSession(ctx, sessionID, owner, 0, stagingKey)
		return Metadata{}, fmt.Errorf("verify upload length: %w", readErr)
	}
	sha := hex.EncodeToString(digest.Sum(nil))
	if request.ExpectedSHA256 != "" && request.ExpectedSHA256 != sha {
		s.failUploadSession(ctx, sessionID, owner, 0, stagingKey)
		return Metadata{}, ErrChecksumMismatch
	}

	claim, err := s.blobs.Claim(uploadCtx, blob.ClaimRequest{SHA256: sha, Size: request.ContentLength, Owner: owner})
	if err != nil {
		s.failUploadSession(ctx, sessionID, owner, 0, stagingKey)
		return Metadata{}, fmt.Errorf("claim uploaded blob: %w", err)
	}
	switch claim.Decision {
	case blob.DecisionReady:
		if s.metrics != nil {
			s.metrics.ObserveBlobDedup(true)
		}
		if err := s.store.Delete(uploadCtx, stagingKey); err != nil {
			s.failUploadSession(ctx, sessionID, owner, 0, stagingKey)
			return Metadata{}, fmt.Errorf("delete deduplicated staging object: %w", err)
		}
	case blob.DecisionOwned:
		if s.metrics != nil {
			s.metrics.ObserveBlobDedup(false)
		}
		if err := s.store.Promote(uploadCtx, stagingKey, claim.ObjectKey, request.ContentLength); err != nil {
			s.failUploadSession(ctx, sessionID, owner, 0, stagingKey)
			return Metadata{}, fmt.Errorf("promote uploaded blob: %w", err)
		}
		if _, err := s.blobs.MarkReady(uploadCtx, blob.Fence{SHA256: sha, Owner: owner, Generation: claim.Generation}); err != nil {
			s.failUploadSession(ctx, sessionID, owner, 0, stagingKey)
			return Metadata{}, fmt.Errorf("mark uploaded blob ready: %w", err)
		}
	default:
		return Metadata{}, fmt.Errorf("unknown blob claim decision %q", claim.Decision)
	}

	metadata, err := s.finalizeUpload(ctx, finalizeUploadRequest{
		SessionID: sessionID, Owner: owner, Repository: repository, Actor: request.Actor,
		RequestID: request.RequestID, Path: path, SHA256: sha, Size: request.ContentLength,
		MediaType: mediaType, Properties: properties,
	})
	if err != nil {
		s.failUploadSession(ctx, sessionID, owner, 0, "")
		return Metadata{}, err
	}
	return metadata, nil
}

func (s *Service) ChecksumDeploy(ctx context.Context, request ChecksumDeployRequest) (Metadata, error) {
	path, err := NormalizePath(request.RawPath)
	if err != nil || !validSHA256(request.SHA256) || request.RequestID == "" {
		return Metadata{}, ErrInvalidRequest
	}
	mediaType := request.MediaType
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	properties, err := encodeProperties(request.Properties)
	if err != nil {
		return Metadata{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Metadata{}, fmt.Errorf("begin checksum deploy: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := db.New(tx)
	repository, err := queries.GetRepositoryByKey(ctx, request.RepositoryKey)
	if err != nil {
		return Metadata{}, mapArtifactDatabaseError("get checksum target repository", err)
	}
	if err := authorizeRepository(request.Actor, auth.ScopeArtifactWrite, repository.ID); err != nil {
		return Metadata{}, err
	}
	if !request.Actor.Scopes.Has(auth.ScopeAdmin) && !request.Actor.Scopes.Has(auth.ScopeArtifactRead) {
		return Metadata{}, ErrNotFound
	}
	visibleIDs := visibleRepositoryIDs(request.Actor)
	_, err = queries.FindVisibleBlobForChecksumDeploy(ctx, db.FindVisibleBlobForChecksumDeployParams{
		Sha256:               request.SHA256,
		VisibleRepositoryIds: visibleIDs,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Metadata{}, ErrNotFound
	}
	if err != nil {
		return Metadata{}, fmt.Errorf("find visible checksum blob: %w", err)
	}
	ready, err := s.blobs.Reference(ctx, tx, request.SHA256)
	if err != nil {
		return Metadata{}, fmt.Errorf("reference checksum blob: %w", err)
	}
	created, err := queries.CreateArtifact(ctx, db.CreateArtifactParams{
		RepositoryID: repository.ID,
		LogicalPath:  path,
		BlobSha256:   request.SHA256,
		MediaType:    mediaType,
		Filename:     filename(path),
		Properties:   properties,
		CreatedBy:    request.Actor.TokenID,
	})
	if err != nil {
		return Metadata{}, mapArtifactDatabaseError("create checksum artifact", err)
	}
	if _, err := s.audit.Record(ctx, tx, audit.Event{
		ActorTokenID: request.Actor.TokenID,
		RepositoryID: repository.ID,
		Action:       "artifact.checksum-deploy",
		ResourceType: "artifact",
		ResourceID:   created.ID.String(),
		Outcome:      audit.OutcomeSuccess,
		RequestID:    request.RequestID,
		Details:      map[string]any{"path": path, "sha256": request.SHA256},
	}); err != nil {
		return Metadata{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Metadata{}, fmt.Errorf("commit checksum deploy: %w", err)
	}
	return metadataFromArtifact(created, repository.Key, ready.Size, request.Properties), nil
}

func (s *Service) Metadata(ctx context.Context, actor auth.Actor, repositoryKey, rawPath string) (Metadata, error) {
	metadata, _, err := s.lookup(ctx, actor, repositoryKey, rawPath)
	return metadata, err
}

func (s *Service) Open(ctx context.Context, request OpenRequest) (OpenResult, error) {
	metadata, objectKey, err := s.lookup(ctx, request.Actor, request.RepositoryKey, request.RawPath)
	if err != nil {
		return OpenResult{}, err
	}
	if request.Redirect == nil || *request.Redirect {
		url, err := s.store.Presign(ctx, objectKey, s.presignTTL)
		if err == nil {
			return OpenResult{Metadata: metadata, RedirectURL: url}, nil
		}
		if !errors.Is(err, storage.ErrPublicEndpointUnavailable) || request.Redirect != nil {
			if errors.Is(err, storage.ErrPublicEndpointUnavailable) {
				return OpenResult{}, ErrPublicEndpointUnavailable
			}
			return OpenResult{}, fmt.Errorf("presign artifact download: %w", err)
		}
	}
	object, err := s.store.Open(ctx, objectKey, "")
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return OpenResult{}, ErrNotFound
		}
		return OpenResult{}, fmt.Errorf("open artifact download: %w", err)
	}
	return OpenResult{Metadata: metadata, Object: object}, nil
}

type finalizeUploadRequest struct {
	SessionID  uuid.UUID
	Owner      uuid.UUID
	Repository db.Repository
	Actor      auth.Actor
	RequestID  string
	Path       string
	SHA256     string
	Size       int64
	MediaType  string
	Properties []byte
}

func (s *Service) finalizeUpload(ctx context.Context, request finalizeUploadRequest) (Metadata, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Metadata{}, fmt.Errorf("begin artifact finalization: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ready, err := s.blobs.Reference(ctx, tx, request.SHA256)
	if err != nil {
		return Metadata{}, fmt.Errorf("reference uploaded blob: %w", err)
	}
	queries := db.New(tx)
	created, err := queries.CreateArtifact(ctx, db.CreateArtifactParams{
		RepositoryID: request.Repository.ID,
		LogicalPath:  request.Path,
		BlobSha256:   request.SHA256,
		MediaType:    request.MediaType,
		Filename:     filename(request.Path),
		Properties:   request.Properties,
		CreatedBy:    request.Actor.TokenID,
	})
	if err != nil {
		return Metadata{}, mapArtifactDatabaseError("create uploaded artifact", err)
	}
	sha := request.SHA256
	size := request.Size
	rows, err := queries.CompleteUploadSession(ctx, db.CompleteUploadSessionParams{
		ID:              request.SessionID,
		LeaseOwner:      request.Owner,
		LeaseGeneration: 0,
		Sha256:          &sha,
		Size:            &size,
		CompletedAt:     pgtype.Timestamptz{Time: s.clock.Now().UTC(), Valid: true},
	})
	if err != nil {
		return Metadata{}, fmt.Errorf("complete upload session: %w", err)
	}
	if rows != 1 {
		return Metadata{}, ErrUploadLeaseLost
	}
	if _, err := s.audit.Record(ctx, tx, audit.Event{
		ActorTokenID: request.Actor.TokenID,
		RepositoryID: request.Repository.ID,
		Action:       "artifact.upload",
		ResourceType: "artifact",
		ResourceID:   created.ID.String(),
		Outcome:      audit.OutcomeSuccess,
		RequestID:    request.RequestID,
		Details:      map[string]any{"path": request.Path, "sha256": request.SHA256, "size": request.Size},
	}); err != nil {
		return Metadata{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Metadata{}, fmt.Errorf("commit artifact finalization: %w", err)
	}
	properties := map[string]any{}
	if err := json.Unmarshal(request.Properties, &properties); err != nil {
		return Metadata{}, fmt.Errorf("decode finalized artifact properties: %w", err)
	}
	return metadataFromArtifact(created, request.Repository.Key, ready.Size, properties), nil
}

func (s *Service) lookup(ctx context.Context, actor auth.Actor, repositoryKey, rawPath string) (Metadata, string, error) {
	path, err := NormalizePath(rawPath)
	if err != nil {
		return Metadata{}, "", ErrInvalidRequest
	}
	queries := db.New(s.pool)
	repository, err := queries.GetRepositoryByKey(ctx, repositoryKey)
	if err != nil {
		return Metadata{}, "", mapArtifactDatabaseError("get artifact repository", err)
	}
	if err := authorizeRepository(actor, auth.ScopeArtifactRead, repository.ID); err != nil {
		return Metadata{}, "", err
	}
	row, err := queries.GetArtifactByPath(ctx, db.GetArtifactByPathParams{RepositoryID: repository.ID, LogicalPath: path})
	if err != nil {
		return Metadata{}, "", mapArtifactDatabaseError("get artifact", err)
	}
	if row.BlobState != string(blob.StateReady) {
		return Metadata{}, "", ErrNotFound
	}
	properties := map[string]any{}
	if err := json.Unmarshal(row.Properties, &properties); err != nil {
		return Metadata{}, "", fmt.Errorf("decode artifact properties: %w", err)
	}
	return Metadata{
		ID:            row.ID,
		RepositoryID:  row.RepositoryID,
		RepositoryKey: repository.Key,
		Path:          row.LogicalPath,
		Filename:      row.Filename,
		MediaType:     row.MediaType,
		Size:          row.Size,
		SHA256:        row.BlobSha256,
		Properties:    properties,
		CreatedBy:     row.CreatedBy,
		CreatedAt:     row.CreatedAt.Time.UTC(),
	}, row.ObjectKey, nil
}

func (s *Service) heartbeat(ctx context.Context, sessionID, owner uuid.UUID, generation int64) error {
	now := s.clock.Now().UTC()
	rows, err := db.New(s.pool).HeartbeatUploadSession(ctx, db.HeartbeatUploadSessionParams{
		ID:              sessionID,
		LeaseOwner:      owner,
		LeaseGeneration: generation,
		LeaseExpiresAt:  pgtype.Timestamptz{Time: now.Add(s.uploadLease), Valid: true},
		LastHeartbeatAt: pgtype.Timestamptz{Time: now, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("heartbeat upload session: %w", err)
	}
	if rows != 1 {
		return ErrUploadLeaseLost
	}
	return nil
}

func (s *Service) failUploadSession(ctx context.Context, sessionID, owner uuid.UUID, generation int64, stagingKey string) {
	_, _ = db.New(s.pool).FailUploadSession(ctx, db.FailUploadSessionParams{
		ID: sessionID, LeaseOwner: owner, LeaseGeneration: generation,
	})
	if stagingKey != "" {
		if err := s.store.Delete(ctx, stagingKey); err == nil || errors.Is(err, storage.ErrNotFound) {
			_, _ = db.New(s.pool).CompleteUploadCleanup(ctx, db.CompleteUploadCleanupParams{
				ID: sessionID, LeaseOwner: owner, LeaseGeneration: generation,
				CleanupCompletedAt: pgtype.Timestamptz{Time: s.clock.Now().UTC(), Valid: true},
			})
		}
	}
}

func authorizeRepository(actor auth.Actor, scope auth.Scope, repositoryID uuid.UUID) error {
	if actor.Scopes.Has(auth.ScopeAdmin) {
		return nil
	}
	if !actor.Scopes.Has(scope) {
		return auth.ErrForbidden
	}
	if _, allowed := actor.RepositoryIDs[repositoryID]; !allowed {
		return errors.Join(ErrNotFound, auth.ErrRepositoryDenied)
	}
	return nil
}

func visibleRepositoryIDs(actor auth.Actor) []uuid.UUID {
	if actor.Scopes.Has(auth.ScopeAdmin) {
		return []uuid.UUID{}
	}
	values := make([]uuid.UUID, 0, len(actor.RepositoryIDs))
	for repositoryID := range actor.RepositoryIDs {
		values = append(values, repositoryID)
	}
	return values
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}

func encodeProperties(properties map[string]any) ([]byte, error) {
	if properties == nil {
		properties = map[string]any{}
	}
	encoded, err := json.Marshal(properties)
	if err != nil {
		return nil, fmt.Errorf("encode artifact properties: %w", err)
	}
	if len(encoded) > 16<<10 {
		return nil, ErrInvalidRequest
	}
	return encoded, nil
}

func filename(path string) string {
	if index := strings.LastIndexByte(path, '/'); index >= 0 {
		return path[index+1:]
	}
	return path
}

func metadataFromArtifact(row db.Artifact, repositoryKey string, size int64, properties map[string]any) Metadata {
	return Metadata{
		ID:            row.ID,
		RepositoryID:  row.RepositoryID,
		RepositoryKey: repositoryKey,
		Path:          row.LogicalPath,
		Filename:      row.Filename,
		MediaType:     row.MediaType,
		Size:          size,
		SHA256:        row.BlobSha256,
		Properties:    properties,
		CreatedBy:     row.CreatedBy,
		CreatedAt:     row.CreatedAt.Time.UTC(),
	}
}

func mapArtifactDatabaseError(operation string, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%s: %w", operation, ErrNotFound)
	}
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		switch postgresError.Code {
		case "23505":
			return fmt.Errorf("%s: %w", operation, ErrConflict)
		case "23502", "23503", "23514", "22P02":
			return fmt.Errorf("%s: %w", operation, ErrInvalidRequest)
		}
	}
	return fmt.Errorf("%s: %w", operation, err)
}

type countingReader struct {
	reader io.Reader
	count  int64
}

type idleTimeoutReader struct {
	reader  io.Reader
	closer  io.Closer
	timeout time.Duration
}

type readResult struct {
	count int
	err   error
}

func (r *idleTimeoutReader) Read(buffer []byte) (int, error) {
	result := make(chan readResult, 1)
	scratch := make([]byte, len(buffer))
	go func() {
		count, err := r.reader.Read(scratch)
		result <- readResult{count: count, err: err}
	}()
	timer := time.NewTimer(r.timeout)
	defer timer.Stop()
	select {
	case completed := <-result:
		copy(buffer, scratch[:completed.count])
		return completed.count, completed.err
	case <-timer.C:
		if r.closer != nil {
			_ = r.closer.Close()
		}
		return 0, ErrUploadIdle
	}
}

func readerCloser(reader io.Reader) io.Closer {
	closer, _ := reader.(io.Closer)
	return closer
}

func (r *countingReader) Read(buffer []byte) (int, error) {
	read, err := r.reader.Read(buffer)
	r.count += int64(read)
	return read, err
}

type heartbeatReader struct {
	reader   io.Reader
	next     time.Time
	interval time.Duration
	now      func() time.Time
	beat     func() error
}

func (r *heartbeatReader) Read(buffer []byte) (int, error) {
	read, err := r.reader.Read(buffer)
	if read > 0 && !r.now().Before(r.next) {
		if heartbeatErr := r.beat(); heartbeatErr != nil {
			return read, heartbeatErr
		}
		r.next = r.now().Add(r.interval)
	}
	return read, err
}
