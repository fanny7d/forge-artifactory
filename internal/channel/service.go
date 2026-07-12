package channel

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"superfan.myasustor.com/fanchao/artifact-repository/internal/audit"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/auth"
	db "superfan.myasustor.com/fanchao/artifact-repository/internal/database/sqlc"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/idempotency"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/storage"
)

var (
	ErrNotFound                  = errors.New("channel not found")
	ErrConflict                  = errors.New("channel conflict")
	ErrInvalidRequest            = errors.New("invalid channel request")
	ErrReleaseNotInPackage       = errors.New("release is not in package")
	ErrReleaseNotPublished       = errors.New("release is not published")
	ErrPublicEndpointUnavailable = storage.ErrPublicEndpointUnavailable
)

const (
	defaultHistoryLimit int32 = 50
	maxHistoryLimit     int32 = 200
)

type Options struct {
	Pool           *pgxpool.Pool
	Idempotency    *idempotency.Service
	Audit          *audit.Service
	Store          storage.Store
	IdempotencyTTL time.Duration
	PresignTTL     time.Duration
}

type Service struct {
	pool           *pgxpool.Pool
	idempotency    *idempotency.Service
	audit          *audit.Service
	store          storage.Store
	idempotencyTTL time.Duration
	presignTTL     time.Duration
}

type PromoteRequest struct {
	Mutation      auth.Mutation
	RepositoryKey string
	PackageName   string
	ChannelName   string
	Version       string
	Reason        string
}

type Revision struct {
	ID          uuid.UUID
	Channel     string
	FromVersion *string
	ToVersion   string
	ActorID     uuid.UUID
	Reason      string
	RequestID   string
	CreatedAt   time.Time
	Replayed    bool
}

type Channel struct {
	ID             uuid.UUID
	RepositoryKey  string
	PackageName    string
	Name           string
	CurrentVersion *string
	CreatedAt      time.Time
}

type HistoryCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

type HistoryRequest struct {
	After *HistoryCursor
	Limit int32
}

type HistoryPage struct {
	Items []Revision
	Next  *HistoryCursor
}

type ResolveRequest struct {
	Actor         auth.Actor
	RepositoryKey string
	PackageName   string
	ChannelName   string
	OS            string
	Arch          string
	Variant       string
	Role          string
	Redirect      *bool
}

type ResolvedArtifact struct {
	Path    string
	OS      string
	Arch    string
	Variant string
	Role    string
	SHA256  string
	Size    int64
}

type Resolution struct {
	Version     string
	Manifest    []byte
	KeyID       string
	Signature   []byte
	Artifact    ResolvedArtifact
	DownloadURL string
}

func NewService(options Options) (*Service, error) {
	if options.Pool == nil || options.Idempotency == nil || options.Audit == nil || options.Store == nil {
		return nil, fmt.Errorf("channel service requires database, idempotency, audit, and storage dependencies")
	}
	if options.IdempotencyTTL <= 0 || options.PresignTTL <= 0 {
		return nil, fmt.Errorf("channel service durations must be positive")
	}
	return &Service{
		pool:           options.Pool,
		idempotency:    options.Idempotency,
		audit:          options.Audit,
		store:          options.Store,
		idempotencyTTL: options.IdempotencyTTL,
		presignTTL:     options.PresignTTL,
	}, nil
}

func (s *Service) Promote(ctx context.Context, request PromoteRequest) (Revision, error) {
	if err := validateMutation(request.Mutation); err != nil {
		return Revision{}, err
	}
	if request.RepositoryKey == "" || request.PackageName == "" || request.Version == "" ||
		len(request.Reason) < 1 || len(request.Reason) > 512 || !validChannel(request.ChannelName) {
		return Revision{}, ErrInvalidRequest
	}
	var repositoryID uuid.UUID
	idempotencyRequest := mutationRequest(request.Mutation, s.idempotencyTTL)
	idempotencyRequest.OnTerminal = func(ctx context.Context, tx pgx.Tx, terminalErr error) error {
		response, ok := classifyMutationError(terminalErr)
		if !ok {
			return nil
		}
		outcome := audit.OutcomeFailed
		if errors.Is(terminalErr, auth.ErrForbidden) || errors.Is(terminalErr, auth.ErrRepositoryDenied) || errors.Is(terminalErr, ErrReleaseNotInPackage) {
			outcome = audit.OutcomeDenied
		}
		code := response.Code
		if errors.Is(terminalErr, auth.ErrRepositoryDenied) {
			code = "repository-not-allowed"
		}
		_, err := s.audit.Record(ctx, tx, audit.Event{
			ActorTokenID: request.Mutation.Actor.TokenID,
			RepositoryID: repositoryID,
			Action:       "channel.promote",
			ResourceType: "channel",
			Outcome:      outcome,
			Code:         code,
			RequestID:    request.Mutation.RequestID,
		})
		return err
	}
	result, err := s.idempotency.RunInTx(ctx, idempotencyRequest, func(ctx context.Context, tx pgx.Tx) (idempotency.Response, error) {
		queries := db.New(tx)
		repository, packageRow, err := s.authorizePackage(ctx, queries, request.Mutation.Actor, request.RepositoryKey, request.PackageName, auth.ScopeChannelPromote)
		if err != nil {
			return idempotency.Response{}, err
		}
		repositoryID = repository.ID
		channelRow, err := queries.GetChannelForUpdate(ctx, db.GetChannelForUpdateParams{PackageID: packageRow.ID, Name: request.ChannelName})
		if err != nil {
			return idempotency.Response{}, mapDatabaseError(err)
		}
		target, err := queries.GetReleaseByVersion(ctx, db.GetReleaseByVersionParams{PackageID: packageRow.ID, Version: request.Version})
		if errors.Is(err, pgx.ErrNoRows) {
			return idempotency.Response{}, ErrReleaseNotInPackage
		}
		if err != nil {
			return idempotency.Response{}, fmt.Errorf("get promotion release: %w", err)
		}
		if target.State != "published" {
			return idempotency.Response{}, ErrReleaseNotPublished
		}
		var fromVersion *string
		if channelRow.CurrentReleaseID != nil {
			previous, err := queries.GetReleaseByID(ctx, *channelRow.CurrentReleaseID)
			if err != nil {
				return idempotency.Response{}, fmt.Errorf("get previous channel release: %w", err)
			}
			if previous.PackageID != packageRow.ID {
				return idempotency.Response{}, ErrConflict
			}
			value := previous.Version
			fromVersion = &value
		}
		revision, err := queries.PromoteChannel(ctx, db.PromoteChannelParams{
			ChannelID:    channelRow.ID,
			PackageID:    packageRow.ID,
			ToReleaseID:  target.ID,
			ActorTokenID: request.Mutation.Actor.TokenID,
			Reason:       request.Reason,
			RequestID:    request.Mutation.RequestID,
		})
		if err != nil {
			return idempotency.Response{}, mapDatabaseError(err)
		}
		value := Revision{
			ID:          revision.ID,
			Channel:     request.ChannelName,
			FromVersion: fromVersion,
			ToVersion:   target.Version,
			ActorID:     revision.ActorTokenID,
			Reason:      revision.Reason,
			RequestID:   revision.RequestID,
			CreatedAt:   revision.CreatedAt.Time.UTC(),
		}
		body, err := json.Marshal(value)
		if err != nil {
			return idempotency.Response{}, fmt.Errorf("encode channel revision: %w", err)
		}
		if _, err := s.audit.Record(ctx, tx, audit.Event{
			ActorTokenID: request.Mutation.Actor.TokenID,
			RepositoryID: repository.ID,
			Action:       "channel.promote",
			ResourceType: "channel",
			ResourceID:   channelRow.ID.String(),
			Outcome:      audit.OutcomeSuccess,
			RequestID:    request.Mutation.RequestID,
			Details:      map[string]any{"version": target.Version, "reason": request.Reason},
		}); err != nil {
			return idempotency.Response{}, err
		}
		return idempotency.Response{Status: 200, Body: body}, nil
	})
	if err != nil {
		return Revision{}, err
	}
	var revision Revision
	if err := json.Unmarshal(result.Body, &revision); err != nil {
		return Revision{}, fmt.Errorf("decode channel revision: %w", err)
	}
	revision.Replayed = result.Replayed
	return revision, nil
}

func (s *Service) Current(ctx context.Context, actor auth.Actor, repositoryKey, packageName, channelName string) (Channel, error) {
	if !validChannel(channelName) {
		return Channel{}, ErrInvalidRequest
	}
	queries := db.New(s.pool)
	repository, packageRow, err := s.authorizePackage(ctx, queries, actor, repositoryKey, packageName, auth.ScopeArtifactRead)
	if err != nil {
		return Channel{}, err
	}
	row, err := queries.GetChannelByName(ctx, db.GetChannelByNameParams{PackageID: packageRow.ID, Name: channelName})
	if err != nil {
		return Channel{}, mapDatabaseError(err)
	}
	var currentVersion *string
	if row.CurrentReleaseID != nil {
		releaseRow, err := queries.GetReleaseByID(ctx, *row.CurrentReleaseID)
		if err != nil {
			return Channel{}, fmt.Errorf("get current channel release: %w", err)
		}
		if releaseRow.PackageID != packageRow.ID || releaseRow.State != "published" {
			return Channel{}, ErrConflict
		}
		value := releaseRow.Version
		currentVersion = &value
	}
	return Channel{
		ID:             row.ID,
		RepositoryKey:  repository.Key,
		PackageName:    packageRow.Name,
		Name:           row.Name,
		CurrentVersion: currentVersion,
		CreatedAt:      row.CreatedAt.Time.UTC(),
	}, nil
}

func (s *Service) History(
	ctx context.Context,
	actor auth.Actor,
	repositoryKey, packageName, channelName string,
	request HistoryRequest,
) (HistoryPage, error) {
	if !validChannel(channelName) {
		return HistoryPage{}, ErrInvalidRequest
	}
	limit, err := historyLimit(request.Limit)
	if err != nil {
		return HistoryPage{}, err
	}
	queries := db.New(s.pool)
	_, packageRow, err := s.authorizePackage(ctx, queries, actor, repositoryKey, packageName, auth.ScopeArtifactRead)
	if err != nil {
		return HistoryPage{}, err
	}
	channelRow, err := queries.GetChannelByName(ctx, db.GetChannelByNameParams{PackageID: packageRow.ID, Name: channelName})
	if err != nil {
		return HistoryPage{}, mapDatabaseError(err)
	}
	params := db.ListChannelHistoryParams{ChannelID: channelRow.ID, AfterID: uuid.Nil, PageLimit: limit + 1}
	if request.After != nil {
		if request.After.ID == uuid.Nil || request.After.CreatedAt.IsZero() {
			return HistoryPage{}, ErrInvalidRequest
		}
		params.AfterCreatedAt = pgtype.Timestamptz{Time: request.After.CreatedAt.UTC(), Valid: true}
		params.AfterID = request.After.ID
	}
	rows, err := queries.ListChannelHistory(ctx, params)
	if err != nil {
		return HistoryPage{}, fmt.Errorf("list channel history: %w", err)
	}
	page := HistoryPage{Items: make([]Revision, 0, min(len(rows), int(limit)))}
	for _, row := range rows[:min(len(rows), int(limit))] {
		page.Items = append(page.Items, Revision{
			ID:          row.ID,
			Channel:     channelRow.Name,
			FromVersion: row.FromVersion,
			ToVersion:   row.ToVersion,
			ActorID:     row.ActorTokenID,
			Reason:      row.Reason,
			RequestID:   row.RequestID,
			CreatedAt:   row.CreatedAt.Time.UTC(),
		})
	}
	if len(rows) > int(limit) {
		last := page.Items[len(page.Items)-1]
		page.Next = &HistoryCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return page, nil
}

func (s *Service) Resolve(ctx context.Context, request ResolveRequest) (Resolution, error) {
	if !validChannel(request.ChannelName) || request.OS == "" || request.Arch == "" ||
		len(request.OS) > 64 || len(request.Arch) > 64 || len(request.Variant) > 64 || len(request.Role) > 64 {
		return Resolution{}, ErrInvalidRequest
	}
	queries := db.New(s.pool)
	repository, packageRow, err := s.authorizePackage(
		ctx,
		queries,
		request.Actor,
		request.RepositoryKey,
		request.PackageName,
		auth.ScopeArtifactRead,
	)
	if err != nil {
		return Resolution{}, err
	}
	row, err := queries.ResolveChannelArtifact(ctx, db.ResolveChannelArtifactParams{
		PackageID: packageRow.ID,
		Name:      request.ChannelName,
		Os:        request.OS,
		Arch:      request.Arch,
		Variant:   request.Variant,
		Role:      request.Role,
	})
	if err != nil {
		return Resolution{}, mapDatabaseError(err)
	}
	manifest, err := readObject(ctx, s.store, row.ManifestObjectKey)
	if err != nil {
		return Resolution{}, fmt.Errorf("read resolved manifest: %w", err)
	}
	signature, err := readObject(ctx, s.store, row.SignatureObjectKey)
	if err != nil {
		return Resolution{}, fmt.Errorf("read resolved signature: %w", err)
	}
	if digestHex(manifest) != row.ManifestBlobSha256 || digestHex(signature) != row.SignatureBlobSha256 {
		return Resolution{}, fmt.Errorf("resolved signed object checksum mismatch")
	}
	if len(row.PublicKey) != ed25519.PublicKeySize || !ed25519.Verify(ed25519.PublicKey(row.PublicKey), manifest, signature) {
		return Resolution{}, fmt.Errorf("resolved manifest signature verification failed")
	}

	downloadURL := proxyArtifactURL(repository.Key, row.LogicalPath)
	if request.Redirect == nil || *request.Redirect {
		signedURL, err := s.store.Presign(ctx, row.ObjectKey, s.presignTTL)
		if err == nil {
			downloadURL = signedURL
		} else if errors.Is(err, storage.ErrPublicEndpointUnavailable) {
			if request.Redirect != nil {
				return Resolution{}, ErrPublicEndpointUnavailable
			}
		} else {
			return Resolution{}, fmt.Errorf("presign resolved artifact: %w", err)
		}
	}
	return Resolution{
		Version:   row.Version,
		Manifest:  manifest,
		KeyID:     row.KeyID,
		Signature: signature,
		Artifact: ResolvedArtifact{
			Path:    row.LogicalPath,
			OS:      row.Os,
			Arch:    row.Arch,
			Variant: row.Variant,
			Role:    row.Role,
			SHA256:  row.BlobSha256,
			Size:    row.Size,
		},
		DownloadURL: downloadURL,
	}, nil
}

func (s *Service) authorizePackage(ctx context.Context, queries *db.Queries, actor auth.Actor, repositoryKey, packageName string, scope auth.Scope) (db.Repository, db.Package, error) {
	repository, err := queries.GetRepositoryByKey(ctx, repositoryKey)
	if err != nil {
		return db.Repository{}, db.Package{}, mapDatabaseError(err)
	}
	if err := authorizeRepository(actor, scope, repository.ID); err != nil {
		return db.Repository{}, db.Package{}, err
	}
	packageRow, err := queries.GetPackageByName(ctx, db.GetPackageByNameParams{Key: repositoryKey, Name: packageName})
	if err != nil {
		return db.Repository{}, db.Package{}, mapDatabaseError(err)
	}
	return repository, packageRow, nil
}

func validateMutation(mutation auth.Mutation) error {
	if mutation.Actor.TokenID == uuid.Nil || mutation.RequestID == "" || mutation.CanonicalResource == "" {
		return ErrInvalidRequest
	}
	if mutation.IdempotencyKey != "" && len(mutation.Fingerprint) != 32 {
		return ErrInvalidRequest
	}
	return nil
}

func mutationRequest(mutation auth.Mutation, ttl time.Duration) idempotency.Request {
	method := mutation.Method
	if method == "" {
		method = "POST"
	}
	return idempotency.Request{
		TokenID:           mutation.Actor.TokenID,
		Method:            method,
		CanonicalResource: mutation.CanonicalResource,
		Key:               mutation.IdempotencyKey,
		Fingerprint:       mutation.Fingerprint,
		TTL:               ttl,
		RequestID:         mutation.RequestID,
		ClassifyError:     classifyMutationError,
	}
}

func classifyMutationError(err error) (idempotency.ErrorResponse, bool) {
	switch {
	case errors.Is(err, auth.ErrForbidden):
		return idempotency.ErrorResponse{Status: 403, Title: "Forbidden", Code: "forbidden"}, true
	case errors.Is(err, auth.ErrRepositoryDenied), errors.Is(err, ErrNotFound):
		return idempotency.ErrorResponse{Status: 404, Title: "Not Found", Code: "not-found"}, true
	case errors.Is(err, ErrInvalidRequest):
		return idempotency.ErrorResponse{Status: 400, Title: "Bad Request", Code: "invalid-request"}, true
	case errors.Is(err, ErrConflict), errors.Is(err, ErrReleaseNotPublished):
		return idempotency.ErrorResponse{Status: 409, Title: "Conflict", Code: "conflict"}, true
	case errors.Is(err, ErrReleaseNotInPackage):
		return idempotency.ErrorResponse{Status: 422, Title: "Unprocessable Entity", Code: "release-not-in-package"}, true
	default:
		return idempotency.ErrorResponse{}, false
	}
}

func authorizeRepository(actor auth.Actor, scope auth.Scope, repositoryID uuid.UUID) error {
	if actor.Scopes.Has(auth.ScopeAdmin) {
		return nil
	}
	if !actor.Scopes.Has(scope) {
		return auth.ErrForbidden
	}
	if _, ok := actor.RepositoryIDs[repositoryID]; !ok {
		return errors.Join(ErrNotFound, auth.ErrRepositoryDenied)
	}
	return nil
}

func validChannel(name string) bool { return name == "candidate" || name == "stable" }

func historyLimit(value int32) (int32, error) {
	if value == 0 {
		return defaultHistoryLimit, nil
	}
	if value < 1 || value > maxHistoryLimit {
		return 0, ErrInvalidRequest
	}
	return value, nil
}

func mapDatabaseError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return fmt.Errorf("channel database operation: %w", err)
}

func proxyArtifactURL(repositoryKey, path string) string {
	segments := strings.Split(path, "/")
	for index, segment := range segments {
		segments[index] = url.PathEscape(segment)
	}
	return "/api/v1/repositories/" + url.PathEscape(repositoryKey) + "/artifacts/" + strings.Join(segments, "/") + "?redirect=false"
}

func readObject(ctx context.Context, store storage.Store, key string) ([]byte, error) {
	object, err := store.Open(ctx, key, "")
	if err != nil {
		return nil, err
	}
	if object.Body == nil {
		return nil, fmt.Errorf("object %q has no body", key)
	}
	defer func() { _ = object.Body.Close() }()
	content, err := io.ReadAll(object.Body)
	if err != nil {
		return nil, fmt.Errorf("read object %q: %w", key, err)
	}
	return content, nil
}

func digestHex(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}
