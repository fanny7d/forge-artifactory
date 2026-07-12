package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"superfan.myasustor.com/fanchao/artifact-repository/internal/audit"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/auth"
	db "superfan.myasustor.com/fanchao/artifact-repository/internal/database/sqlc"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/idempotency"
)

const (
	releaseDefaultPageLimit int32 = 50
	releaseMaxPageLimit     int32 = 200
)

var (
	ErrNotFound       = errors.New("release resource not found")
	ErrConflict       = errors.New("release resource conflict")
	ErrInvalidRequest = errors.New("invalid release request")
	ErrUnprocessable  = errors.New("release request violates ownership constraints")
)

type PackageServiceOptions struct {
	Pool           *pgxpool.Pool
	Idempotency    *idempotency.Service
	Audit          *audit.Service
	IdempotencyTTL time.Duration
}

type PackageService struct {
	pool           *pgxpool.Pool
	idempotency    *idempotency.Service
	audit          *audit.Service
	idempotencyTTL time.Duration
}

type CreatePackageRequest struct {
	Mutation      auth.Mutation
	RepositoryKey string
	Name          string
}

type Package struct {
	ID            uuid.UUID `json:"id"`
	RepositoryID  uuid.UUID `json:"repositoryId"`
	RepositoryKey string    `json:"repository"`
	Name          string    `json:"name"`
	Channels      []string  `json:"channels"`
	CreatedAt     time.Time `json:"createdAt"`
	Replayed      bool      `json:"-"`
}

type PackageCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

type PackageListRequest struct {
	After *PackageCursor
	Limit int32
}

type PackagePage struct {
	Items []Package
	Next  *PackageCursor
}

func NewPackageService(options PackageServiceOptions) (*PackageService, error) {
	if options.Pool == nil {
		return nil, fmt.Errorf("package service: pool is nil")
	}
	if options.Idempotency == nil {
		return nil, fmt.Errorf("package service: idempotency service is nil")
	}
	if options.Audit == nil {
		return nil, fmt.Errorf("package service: audit service is nil")
	}
	if options.IdempotencyTTL <= 0 {
		return nil, fmt.Errorf("package service: idempotency TTL must be positive")
	}
	return &PackageService{
		pool:           options.Pool,
		idempotency:    options.Idempotency,
		audit:          options.Audit,
		idempotencyTTL: options.IdempotencyTTL,
	}, nil
}

func (s *PackageService) Create(ctx context.Context, request CreatePackageRequest) (Package, error) {
	if err := validateMutation(request.Mutation); err != nil {
		return Package{}, err
	}
	var terminalRepositoryID uuid.UUID
	idempotencyRequest := withReleaseTerminalAudit(
		mutationIdempotencyRequest(request.Mutation, s.idempotencyTTL),
		s.audit,
		request.Mutation,
		"package.create",
		"package",
		&terminalRepositoryID,
	)
	result, err := s.idempotency.RunInTx(ctx, idempotencyRequest, func(ctx context.Context, tx pgx.Tx) (idempotency.Response, error) {
		queries := db.New(tx)
		repository, err := queries.GetRepositoryByKey(ctx, request.RepositoryKey)
		if err != nil {
			return idempotency.Response{}, mapReleaseDatabaseError("get package repository", err)
		}
		terminalRepositoryID = repository.ID
		if err := authorizeRepository(request.Mutation.Actor, auth.ScopeReleasePublish, repository.ID); err != nil {
			return idempotency.Response{}, err
		}
		created, err := queries.CreatePackage(ctx, db.CreatePackageParams{
			RepositoryID: repository.ID,
			Name:         request.Name,
			CreatedBy:    request.Mutation.Actor.TokenID,
		})
		if err != nil {
			return idempotency.Response{}, mapReleaseDatabaseError("create package", err)
		}
		channels := make([]string, 0, 2)
		for _, name := range []string{"candidate", "stable"} {
			channel, err := queries.CreateDefaultChannel(ctx, db.CreateDefaultChannelParams{
				PackageID: created.ID,
				Name:      name,
			})
			if err != nil {
				return idempotency.Response{}, mapReleaseDatabaseError("create default channel", err)
			}
			channels = append(channels, channel.Name)
		}
		value := packageFromRow(created, repository.Key, channels)
		if _, err := s.audit.Record(ctx, tx, audit.Event{
			ActorTokenID: request.Mutation.Actor.TokenID,
			RepositoryID: repository.ID,
			Action:       "package.create",
			ResourceType: "package",
			ResourceID:   created.ID.String(),
			Outcome:      audit.OutcomeSuccess,
			RequestID:    request.Mutation.RequestID,
			Details: map[string]any{
				"repository": repository.Key,
				"name":       created.Name,
			},
		}); err != nil {
			return idempotency.Response{}, err
		}
		body, err := json.Marshal(value)
		if err != nil {
			return idempotency.Response{}, fmt.Errorf("encode package response: %w", err)
		}
		return idempotency.Response{Status: 201, Body: body}, nil
	})
	if err != nil {
		return Package{}, err
	}
	var created Package
	if err := json.Unmarshal(result.Body, &created); err != nil {
		return Package{}, fmt.Errorf("decode package response: %w", err)
	}
	created.Replayed = result.Replayed
	return created, nil
}

func (s *PackageService) Get(ctx context.Context, actor auth.Actor, repositoryKey, name string) (Package, error) {
	queries := db.New(s.pool)
	repository, err := queries.GetRepositoryByKey(ctx, repositoryKey)
	if err != nil {
		return Package{}, mapReleaseDatabaseError("get package repository", err)
	}
	if err := authorizeRepository(actor, auth.ScopeArtifactRead, repository.ID); err != nil {
		return Package{}, err
	}
	row, err := queries.GetPackageByName(ctx, db.GetPackageByNameParams{Key: repositoryKey, Name: name})
	if err != nil {
		return Package{}, mapReleaseDatabaseError("get package", err)
	}
	channels, err := packageChannels(ctx, queries, row.ID)
	if err != nil {
		return Package{}, err
	}
	return packageFromRow(row, repository.Key, channels), nil
}

func (s *PackageService) List(ctx context.Context, actor auth.Actor, repositoryKey string, request PackageListRequest) (PackagePage, error) {
	limit, err := normalizeReleasePageLimit(request.Limit)
	if err != nil {
		return PackagePage{}, err
	}
	queries := db.New(s.pool)
	repository, err := queries.GetRepositoryByKey(ctx, repositoryKey)
	if err != nil {
		return PackagePage{}, mapReleaseDatabaseError("get package repository", err)
	}
	if err := authorizeRepository(actor, auth.ScopeArtifactRead, repository.ID); err != nil {
		return PackagePage{}, err
	}
	params := db.ListPackagesParams{
		RepositoryID: repository.ID,
		AfterID:      uuid.Nil,
		PageLimit:    limit + 1,
	}
	if request.After != nil {
		if request.After.ID == uuid.Nil || request.After.CreatedAt.IsZero() {
			return PackagePage{}, ErrInvalidRequest
		}
		params.AfterCreatedAt = pgtype.Timestamptz{Time: request.After.CreatedAt, Valid: true}
		params.AfterID = request.After.ID
	}
	rows, err := queries.ListPackages(ctx, params)
	if err != nil {
		return PackagePage{}, fmt.Errorf("list packages: %w", err)
	}
	pageRows := rows[:min(len(rows), int(limit))]
	page := PackagePage{Items: make([]Package, 0, len(pageRows))}
	for _, row := range pageRows {
		channels, err := packageChannels(ctx, queries, row.ID)
		if err != nil {
			return PackagePage{}, err
		}
		page.Items = append(page.Items, packageFromRow(row, repository.Key, channels))
	}
	if len(rows) > int(limit) {
		last := page.Items[len(page.Items)-1]
		page.Next = &PackageCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return page, nil
}

func packageChannels(ctx context.Context, queries *db.Queries, packageID uuid.UUID) ([]string, error) {
	rows, err := queries.ListChannelsByPackage(ctx, packageID)
	if err != nil {
		return nil, fmt.Errorf("list package channels: %w", err)
	}
	channels := make([]string, len(rows))
	for index, row := range rows {
		channels[index] = row.Name
	}
	return channels, nil
}

func packageFromRow(row db.Package, repositoryKey string, channels []string) Package {
	return Package{
		ID:            row.ID,
		RepositoryID:  row.RepositoryID,
		RepositoryKey: repositoryKey,
		Name:          row.Name,
		Channels:      channels,
		CreatedAt:     row.CreatedAt.Time.UTC(),
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

func validateMutation(mutation auth.Mutation) error {
	if mutation.Actor.TokenID == uuid.Nil || mutation.RequestID == "" || mutation.CanonicalResource == "" {
		return ErrInvalidRequest
	}
	if mutation.IdempotencyKey != "" && len(mutation.Fingerprint) != 32 {
		return ErrInvalidRequest
	}
	return nil
}

func mutationIdempotencyRequest(mutation auth.Mutation, ttl time.Duration) idempotency.Request {
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
		ClassifyError:     classifyReleaseMutationError,
	}
}

func classifyReleaseMutationError(err error) (idempotency.ErrorResponse, bool) {
	switch {
	case errors.Is(err, auth.ErrForbidden):
		return idempotency.ErrorResponse{Status: 403, Title: "Forbidden", Code: "forbidden"}, true
	case errors.Is(err, auth.ErrRepositoryDenied), errors.Is(err, ErrNotFound):
		return idempotency.ErrorResponse{Status: 404, Title: "Not Found", Code: "not-found"}, true
	case errors.Is(err, ErrInvalidRequest):
		return idempotency.ErrorResponse{Status: 400, Title: "Bad Request", Code: "invalid-request"}, true
	case errors.Is(err, ErrConflict):
		return idempotency.ErrorResponse{Status: 409, Title: "Conflict", Code: "conflict"}, true
	case errors.Is(err, ErrUnprocessable):
		return idempotency.ErrorResponse{Status: 422, Title: "Unprocessable Entity", Code: "cross-repository-artifact"}, true
	default:
		return idempotency.ErrorResponse{}, false
	}
}

func withReleaseTerminalAudit(
	request idempotency.Request,
	service *audit.Service,
	mutation auth.Mutation,
	action string,
	resourceType string,
	repositoryID *uuid.UUID,
) idempotency.Request {
	request.OnTerminal = func(ctx context.Context, tx pgx.Tx, terminalErr error) error {
		response, ok := classifyReleaseMutationError(terminalErr)
		if !ok {
			return nil
		}
		outcome := audit.OutcomeFailed
		if errors.Is(terminalErr, auth.ErrForbidden) || errors.Is(terminalErr, auth.ErrRepositoryDenied) || errors.Is(terminalErr, ErrUnprocessable) {
			outcome = audit.OutcomeDenied
		}
		code := response.Code
		if errors.Is(terminalErr, auth.ErrRepositoryDenied) {
			code = "repository-not-allowed"
		}
		_, err := service.Record(ctx, tx, audit.Event{
			ActorTokenID: mutation.Actor.TokenID,
			RepositoryID: *repositoryID,
			Action:       action,
			ResourceType: resourceType,
			Outcome:      outcome,
			Code:         code,
			RequestID:    mutation.RequestID,
		})
		return err
	}
	return request
}

func normalizeReleasePageLimit(value int32) (int32, error) {
	if value == 0 {
		return releaseDefaultPageLimit, nil
	}
	if value < 1 || value > releaseMaxPageLimit {
		return 0, ErrInvalidRequest
	}
	return value, nil
}

func mapReleaseDatabaseError(operation string, err error) error {
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
