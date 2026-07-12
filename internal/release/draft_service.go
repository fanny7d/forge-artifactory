package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"superfan.myasustor.com/fanchao/artifact-repository/internal/audit"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/auth"
	db "superfan.myasustor.com/fanchao/artifact-repository/internal/database/sqlc"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/idempotency"
)

type DraftServiceOptions struct {
	Pool           *pgxpool.Pool
	Idempotency    *idempotency.Service
	Audit          *audit.Service
	IdempotencyTTL time.Duration
}

type DraftService struct {
	pool           *pgxpool.Pool
	idempotency    *idempotency.Service
	audit          *audit.Service
	idempotencyTTL time.Duration
}

type CreateDraftRequest struct {
	Mutation      auth.Mutation
	RepositoryKey string
	PackageName   string
	Version       string
}

type AddArtifactRequest struct {
	Mutation      auth.Mutation
	RepositoryKey string
	PackageName   string
	Version       string
	ArtifactPath  string
	OS            string
	Arch          string
	Variant       string
	Role          string
}

type RemoveArtifactRequest struct {
	Mutation          auth.Mutation
	RepositoryKey     string
	PackageName       string
	Version           string
	ReleaseArtifactID uuid.UUID
}

type CancelDraftRequest struct {
	Mutation      auth.Mutation
	RepositoryKey string
	PackageName   string
	Version       string
}

type Artifact struct {
	ID            uuid.UUID      `json:"id"`
	RepositoryID  uuid.UUID      `json:"repositoryId"`
	RepositoryKey string         `json:"repository"`
	Path          string         `json:"path"`
	Filename      string         `json:"filename"`
	MediaType     string         `json:"mediaType"`
	Size          int64          `json:"size"`
	SHA256        string         `json:"sha256"`
	Properties    map[string]any `json:"properties"`
	CreatedBy     uuid.UUID      `json:"createdBy"`
	CreatedAt     time.Time      `json:"createdAt"`
}

type ReleaseArtifact struct {
	ID       uuid.UUID `json:"id"`
	Artifact Artifact  `json:"artifact"`
	OS       string    `json:"os"`
	Arch     string    `json:"arch"`
	Variant  string    `json:"variant"`
	Role     string    `json:"role"`
	Replayed bool      `json:"-"`
}

type Release struct {
	ID            uuid.UUID         `json:"id"`
	RepositoryID  uuid.UUID         `json:"repositoryId"`
	RepositoryKey string            `json:"repository"`
	PackageID     uuid.UUID         `json:"packageId"`
	PackageName   string            `json:"package"`
	Version       string            `json:"version"`
	State         string            `json:"state"`
	Artifacts     []ReleaseArtifact `json:"artifacts"`
	PublishedAt   *time.Time        `json:"publishedAt,omitempty"`
	FailureCode   *string           `json:"failureCode,omitempty"`
	CreatedAt     time.Time         `json:"createdAt"`
	Replayed      bool              `json:"-"`
}

type ReleaseCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

type ReleaseListRequest struct {
	After *ReleaseCursor
	Limit int32
}

type ReleasePage struct {
	Items []Release
	Next  *ReleaseCursor
}

type RemoveArtifactResult struct {
	Replayed bool
}

type CancelDraftResult struct {
	Replayed bool
}

func NewDraftService(options DraftServiceOptions) (*DraftService, error) {
	if options.Pool == nil {
		return nil, fmt.Errorf("draft service: pool is nil")
	}
	if options.Idempotency == nil {
		return nil, fmt.Errorf("draft service: idempotency service is nil")
	}
	if options.Audit == nil {
		return nil, fmt.Errorf("draft service: audit service is nil")
	}
	if options.IdempotencyTTL <= 0 {
		return nil, fmt.Errorf("draft service: idempotency TTL must be positive")
	}
	return &DraftService{
		pool:           options.Pool,
		idempotency:    options.Idempotency,
		audit:          options.Audit,
		idempotencyTTL: options.IdempotencyTTL,
	}, nil
}

func (s *DraftService) Create(ctx context.Context, request CreateDraftRequest) (Release, error) {
	if err := validateMutation(request.Mutation); err != nil {
		return Release{}, err
	}
	var terminalRepositoryID uuid.UUID
	idempotencyRequest := withReleaseTerminalAudit(
		mutationIdempotencyRequest(request.Mutation, s.idempotencyTTL),
		s.audit,
		request.Mutation,
		"release.create",
		"release",
		&terminalRepositoryID,
	)
	result, err := s.idempotency.RunInTx(ctx, idempotencyRequest, func(ctx context.Context, tx pgx.Tx) (idempotency.Response, error) {
		queries := db.New(tx)
		repository, packageRow, err := releasePackage(ctx, queries, request.Mutation.Actor, auth.ScopeReleasePublish, request.RepositoryKey, request.PackageName)
		if err != nil {
			return idempotency.Response{}, err
		}
		terminalRepositoryID = repository.ID
		created, err := queries.CreateRelease(ctx, db.CreateReleaseParams{
			PackageID: packageRow.ID,
			Version:   request.Version,
			CreatedBy: request.Mutation.Actor.TokenID,
		})
		if err != nil {
			return idempotency.Response{}, mapReleaseDatabaseError("create draft release", err)
		}
		value := releaseFromRow(created, repository, packageRow, []ReleaseArtifact{})
		if _, err := s.audit.Record(ctx, tx, audit.Event{
			ActorTokenID: request.Mutation.Actor.TokenID,
			RepositoryID: repository.ID,
			Action:       "release.create",
			ResourceType: "release",
			ResourceID:   created.ID.String(),
			Outcome:      audit.OutcomeSuccess,
			RequestID:    request.Mutation.RequestID,
			Details: map[string]any{
				"package": packageRow.Name,
				"version": created.Version,
			},
		}); err != nil {
			return idempotency.Response{}, err
		}
		body, err := json.Marshal(value)
		if err != nil {
			return idempotency.Response{}, fmt.Errorf("encode draft release response: %w", err)
		}
		return idempotency.Response{Status: 201, Body: body}, nil
	})
	if err != nil {
		return Release{}, err
	}
	var created Release
	if err := json.Unmarshal(result.Body, &created); err != nil {
		return Release{}, fmt.Errorf("decode draft release response: %w", err)
	}
	created.Replayed = result.Replayed
	return created, nil
}

func (s *DraftService) AddArtifact(ctx context.Context, request AddArtifactRequest) (ReleaseArtifact, error) {
	if err := validateMutation(request.Mutation); err != nil {
		return ReleaseArtifact{}, err
	}
	var terminalRepositoryID uuid.UUID
	idempotencyRequest := withReleaseTerminalAudit(
		mutationIdempotencyRequest(request.Mutation, s.idempotencyTTL),
		s.audit,
		request.Mutation,
		"release-artifact.add",
		"release_artifact",
		&terminalRepositoryID,
	)
	result, err := s.idempotency.RunInTx(ctx, idempotencyRequest, func(ctx context.Context, tx pgx.Tx) (idempotency.Response, error) {
		queries := db.New(tx)
		repository, packageRow, err := releasePackage(ctx, queries, request.Mutation.Actor, auth.ScopeReleasePublish, request.RepositoryKey, request.PackageName)
		if err != nil {
			return idempotency.Response{}, err
		}
		terminalRepositoryID = repository.ID
		releaseRow, err := queries.GetReleaseForUpdate(ctx, db.GetReleaseForUpdateParams{PackageID: packageRow.ID, Version: request.Version})
		if err != nil {
			return idempotency.Response{}, mapReleaseDatabaseError("get draft release", err)
		}
		if releaseRow.State != "draft" {
			return idempotency.Response{}, ErrConflict
		}
		artifactRow, err := queries.GetArtifactByPath(ctx, db.GetArtifactByPathParams{
			RepositoryID: repository.ID,
			LogicalPath:  request.ArtifactPath,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			visibleRepositoryIDs := visibleRepositoryIDs(request.Mutation.Actor)
			_, outsideErr := queries.FindVisibleArtifactOutsideRepository(ctx, db.FindVisibleArtifactOutsideRepositoryParams{
				LogicalPath:          request.ArtifactPath,
				TargetRepositoryID:   repository.ID,
				VisibleRepositoryIds: visibleRepositoryIDs,
			})
			switch {
			case outsideErr == nil:
				return idempotency.Response{}, ErrUnprocessable
			case errors.Is(outsideErr, pgx.ErrNoRows):
				return idempotency.Response{}, ErrNotFound
			default:
				return idempotency.Response{}, fmt.Errorf("find visible cross-repository artifact: %w", outsideErr)
			}
		}
		if err != nil {
			return idempotency.Response{}, mapReleaseDatabaseError("get release artifact", err)
		}
		if artifactRow.BlobState != "ready" {
			return idempotency.Response{}, ErrConflict
		}
		created, err := queries.AddReleaseArtifact(ctx, db.AddReleaseArtifactParams{
			ReleaseID:  releaseRow.ID,
			ArtifactID: artifactRow.ID,
			Os:         request.OS,
			Arch:       request.Arch,
			Variant:    request.Variant,
			Role:       request.Role,
		})
		if err != nil {
			return idempotency.Response{}, mapReleaseDatabaseError("add release artifact", err)
		}
		artifact, err := artifactFromGetRow(artifactRow, repository.Key)
		if err != nil {
			return idempotency.Response{}, err
		}
		value := ReleaseArtifact{
			ID:       created.ID,
			Artifact: artifact,
			OS:       created.Os,
			Arch:     created.Arch,
			Variant:  created.Variant,
			Role:     created.Role,
		}
		if _, err := s.audit.Record(ctx, tx, audit.Event{
			ActorTokenID: request.Mutation.Actor.TokenID,
			RepositoryID: repository.ID,
			Action:       "release-artifact.add",
			ResourceType: "release_artifact",
			ResourceID:   created.ID.String(),
			Outcome:      audit.OutcomeSuccess,
			RequestID:    request.Mutation.RequestID,
			Details: map[string]any{
				"releaseId":    releaseRow.ID.String(),
				"artifactPath": artifactRow.LogicalPath,
				"os":           created.Os,
				"arch":         created.Arch,
				"variant":      created.Variant,
				"role":         created.Role,
			},
		}); err != nil {
			return idempotency.Response{}, err
		}
		body, err := json.Marshal(value)
		if err != nil {
			return idempotency.Response{}, fmt.Errorf("encode release artifact response: %w", err)
		}
		return idempotency.Response{Status: 201, Body: body}, nil
	})
	if err != nil {
		return ReleaseArtifact{}, err
	}
	var created ReleaseArtifact
	if err := json.Unmarshal(result.Body, &created); err != nil {
		return ReleaseArtifact{}, fmt.Errorf("decode release artifact response: %w", err)
	}
	created.Replayed = result.Replayed
	return created, nil
}

func (s *DraftService) RemoveArtifact(ctx context.Context, request RemoveArtifactRequest) (RemoveArtifactResult, error) {
	if err := validateMutation(request.Mutation); err != nil {
		return RemoveArtifactResult{}, err
	}
	if request.ReleaseArtifactID == uuid.Nil {
		return RemoveArtifactResult{}, ErrInvalidRequest
	}
	var terminalRepositoryID uuid.UUID
	idempotencyRequest := withReleaseTerminalAudit(
		mutationIdempotencyRequest(request.Mutation, s.idempotencyTTL),
		s.audit,
		request.Mutation,
		"release-artifact.remove",
		"release_artifact",
		&terminalRepositoryID,
	)
	result, err := s.idempotency.RunInTx(ctx, idempotencyRequest, func(ctx context.Context, tx pgx.Tx) (idempotency.Response, error) {
		queries := db.New(tx)
		repository, packageRow, err := releasePackage(ctx, queries, request.Mutation.Actor, auth.ScopeReleasePublish, request.RepositoryKey, request.PackageName)
		if err != nil {
			return idempotency.Response{}, err
		}
		terminalRepositoryID = repository.ID
		releaseRow, err := queries.GetReleaseForUpdate(ctx, db.GetReleaseForUpdateParams{PackageID: packageRow.ID, Version: request.Version})
		if err != nil {
			return idempotency.Response{}, mapReleaseDatabaseError("get draft release", err)
		}
		if releaseRow.State != "draft" {
			return idempotency.Response{}, ErrConflict
		}
		rows, err := queries.RemoveReleaseArtifact(ctx, db.RemoveReleaseArtifactParams{
			ReleaseArtifactID: request.ReleaseArtifactID,
			ReleaseID:         releaseRow.ID,
		})
		if err != nil {
			return idempotency.Response{}, fmt.Errorf("remove release artifact: %w", err)
		}
		if rows == 0 {
			return idempotency.Response{}, ErrNotFound
		}
		if _, err := s.audit.Record(ctx, tx, audit.Event{
			ActorTokenID: request.Mutation.Actor.TokenID,
			RepositoryID: repository.ID,
			Action:       "release-artifact.remove",
			ResourceType: "release_artifact",
			ResourceID:   request.ReleaseArtifactID.String(),
			Outcome:      audit.OutcomeSuccess,
			RequestID:    request.Mutation.RequestID,
			Details:      map[string]any{"releaseId": releaseRow.ID.String()},
		}); err != nil {
			return idempotency.Response{}, err
		}
		return idempotency.Response{Status: 204, Body: []byte{}}, nil
	})
	if err != nil {
		return RemoveArtifactResult{}, err
	}
	return RemoveArtifactResult{Replayed: result.Replayed}, nil
}

func (s *DraftService) Cancel(ctx context.Context, request CancelDraftRequest) (CancelDraftResult, error) {
	if err := validateMutation(request.Mutation); err != nil {
		return CancelDraftResult{}, err
	}
	var terminalRepositoryID uuid.UUID
	idempotencyRequest := withReleaseTerminalAudit(
		mutationIdempotencyRequest(request.Mutation, s.idempotencyTTL),
		s.audit,
		request.Mutation,
		"release.cancel",
		"release",
		&terminalRepositoryID,
	)
	result, err := s.idempotency.RunInTx(ctx, idempotencyRequest, func(ctx context.Context, tx pgx.Tx) (idempotency.Response, error) {
		queries := db.New(tx)
		repository, packageRow, err := releasePackage(ctx, queries, request.Mutation.Actor, auth.ScopeReleasePublish, request.RepositoryKey, request.PackageName)
		if err != nil {
			return idempotency.Response{}, err
		}
		terminalRepositoryID = repository.ID
		releaseRow, err := queries.GetReleaseForUpdate(ctx, db.GetReleaseForUpdateParams{PackageID: packageRow.ID, Version: request.Version})
		if err != nil {
			return idempotency.Response{}, mapReleaseDatabaseError("get draft release", err)
		}
		if releaseRow.State != "draft" {
			return idempotency.Response{}, ErrConflict
		}
		rows, err := queries.CancelDraftRelease(ctx, releaseRow.ID)
		if err != nil {
			return idempotency.Response{}, fmt.Errorf("cancel draft release: %w", err)
		}
		if rows == 0 {
			return idempotency.Response{}, ErrConflict
		}
		if _, err := s.audit.Record(ctx, tx, audit.Event{
			ActorTokenID: request.Mutation.Actor.TokenID,
			RepositoryID: repository.ID,
			Action:       "release.cancel",
			ResourceType: "release",
			ResourceID:   releaseRow.ID.String(),
			Outcome:      audit.OutcomeSuccess,
			RequestID:    request.Mutation.RequestID,
			Details: map[string]any{
				"package": packageRow.Name,
				"version": releaseRow.Version,
			},
		}); err != nil {
			return idempotency.Response{}, err
		}
		return idempotency.Response{Status: 204, Body: []byte{}}, nil
	})
	if err != nil {
		return CancelDraftResult{}, err
	}
	return CancelDraftResult{Replayed: result.Replayed}, nil
}

func (s *DraftService) Get(ctx context.Context, actor auth.Actor, repositoryKey, packageName, version string) (Release, error) {
	queries := db.New(s.pool)
	repository, packageRow, err := releasePackage(ctx, queries, actor, auth.ScopeArtifactRead, repositoryKey, packageName)
	if err != nil {
		return Release{}, err
	}
	row, err := queries.GetReleaseByVersion(ctx, db.GetReleaseByVersionParams{PackageID: packageRow.ID, Version: version})
	if err != nil {
		return Release{}, mapReleaseDatabaseError("get release", err)
	}
	artifacts, err := releaseArtifacts(ctx, queries, row.ID, repository.Key)
	if err != nil {
		return Release{}, err
	}
	return releaseFromRow(row, repository, packageRow, artifacts), nil
}

func (s *DraftService) List(ctx context.Context, actor auth.Actor, repositoryKey, packageName string, request ReleaseListRequest) (ReleasePage, error) {
	limit, err := normalizeReleasePageLimit(request.Limit)
	if err != nil {
		return ReleasePage{}, err
	}
	queries := db.New(s.pool)
	repository, packageRow, err := releasePackage(ctx, queries, actor, auth.ScopeArtifactRead, repositoryKey, packageName)
	if err != nil {
		return ReleasePage{}, err
	}
	params := db.ListReleasesParams{PackageID: packageRow.ID, AfterID: uuid.Nil, PageLimit: limit + 1}
	if request.After != nil {
		if request.After.ID == uuid.Nil || request.After.CreatedAt.IsZero() {
			return ReleasePage{}, ErrInvalidRequest
		}
		params.AfterCreatedAt = pgtype.Timestamptz{Time: request.After.CreatedAt, Valid: true}
		params.AfterID = request.After.ID
	}
	rows, err := queries.ListReleases(ctx, params)
	if err != nil {
		return ReleasePage{}, fmt.Errorf("list releases: %w", err)
	}
	pageRows := rows[:min(len(rows), int(limit))]
	page := ReleasePage{Items: make([]Release, 0, len(pageRows))}
	for _, row := range pageRows {
		artifacts, err := releaseArtifacts(ctx, queries, row.ID, repository.Key)
		if err != nil {
			return ReleasePage{}, err
		}
		page.Items = append(page.Items, releaseFromRow(row, repository, packageRow, artifacts))
	}
	if len(rows) > int(limit) {
		last := page.Items[len(page.Items)-1]
		page.Next = &ReleaseCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return page, nil
}

func releasePackage(
	ctx context.Context,
	queries *db.Queries,
	actor auth.Actor,
	scope auth.Scope,
	repositoryKey string,
	packageName string,
) (db.Repository, db.Package, error) {
	repository, err := queries.GetRepositoryByKey(ctx, repositoryKey)
	if err != nil {
		return db.Repository{}, db.Package{}, mapReleaseDatabaseError("get release repository", err)
	}
	if err := authorizeRepository(actor, scope, repository.ID); err != nil {
		return db.Repository{}, db.Package{}, err
	}
	packageRow, err := queries.GetPackageByName(ctx, db.GetPackageByNameParams{Key: repositoryKey, Name: packageName})
	if err != nil {
		return db.Repository{}, db.Package{}, mapReleaseDatabaseError("get release package", err)
	}
	return repository, packageRow, nil
}

func releaseArtifacts(ctx context.Context, queries *db.Queries, releaseID uuid.UUID, repositoryKey string) ([]ReleaseArtifact, error) {
	rows, err := queries.ListReleaseArtifacts(ctx, releaseID)
	if err != nil {
		return nil, fmt.Errorf("list release artifacts: %w", err)
	}
	items := make([]ReleaseArtifact, 0, len(rows))
	for _, row := range rows {
		artifact, err := artifactFromListRow(row, repositoryKey)
		if err != nil {
			return nil, err
		}
		items = append(items, ReleaseArtifact{
			ID:       row.ID,
			Artifact: artifact,
			OS:       row.Os,
			Arch:     row.Arch,
			Variant:  row.Variant,
			Role:     row.Role,
		})
	}
	return items, nil
}

func artifactFromGetRow(row db.GetArtifactByPathRow, repositoryKey string) (Artifact, error) {
	properties, err := decodeProperties(row.Properties, row.ID)
	if err != nil {
		return Artifact{}, err
	}
	return Artifact{
		ID:            row.ID,
		RepositoryID:  row.RepositoryID,
		RepositoryKey: repositoryKey,
		Path:          row.LogicalPath,
		Filename:      row.Filename,
		MediaType:     row.MediaType,
		Size:          row.Size,
		SHA256:        row.BlobSha256,
		Properties:    properties,
		CreatedBy:     row.CreatedBy,
		CreatedAt:     row.CreatedAt.Time.UTC(),
	}, nil
}

func artifactFromListRow(row db.ListReleaseArtifactsRow, repositoryKey string) (Artifact, error) {
	properties, err := decodeProperties(row.Properties, row.ArtifactID)
	if err != nil {
		return Artifact{}, err
	}
	return Artifact{
		ID:            row.ArtifactID,
		RepositoryID:  row.RepositoryID,
		RepositoryKey: repositoryKey,
		Path:          row.LogicalPath,
		Filename:      row.Filename,
		MediaType:     row.MediaType,
		Size:          row.Size,
		SHA256:        row.BlobSha256,
		Properties:    properties,
		CreatedBy:     row.ArtifactCreatedBy,
		CreatedAt:     row.ArtifactCreatedAt.Time.UTC(),
	}, nil
}

func decodeProperties(encoded []byte, artifactID uuid.UUID) (map[string]any, error) {
	properties := map[string]any{}
	if err := json.Unmarshal(encoded, &properties); err != nil {
		return nil, fmt.Errorf("decode artifact %s properties: %w", artifactID, err)
	}
	return properties, nil
}

func releaseFromRow(row db.Release, repository db.Repository, packageRow db.Package, artifacts []ReleaseArtifact) Release {
	var publishedAt *time.Time
	if row.PublishedAt.Valid {
		value := row.PublishedAt.Time.UTC()
		publishedAt = &value
	}
	return Release{
		ID:            row.ID,
		RepositoryID:  repository.ID,
		RepositoryKey: repository.Key,
		PackageID:     packageRow.ID,
		PackageName:   packageRow.Name,
		Version:       row.Version,
		State:         row.State,
		Artifacts:     artifacts,
		PublishedAt:   publishedAt,
		FailureCode:   row.FailureCode,
		CreatedAt:     row.CreatedAt.Time.UTC(),
	}
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
