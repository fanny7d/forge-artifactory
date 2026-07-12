package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"superfan.myasustor.com/fanchao/artifact-repository/internal/audit"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/auth"
	db "superfan.myasustor.com/fanchao/artifact-repository/internal/database/sqlc"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/idempotency"
)

const (
	defaultPageLimit int32 = 50
	maxPageLimit     int32 = 200
)

var (
	ErrNotFound       = errors.New("repository not found")
	ErrConflict       = errors.New("repository conflict")
	ErrInvalidRequest = errors.New("invalid repository request")
)

type Options struct {
	Pool           *pgxpool.Pool
	Idempotency    *idempotency.Service
	Audit          *audit.Service
	IdempotencyTTL time.Duration
}

type Service struct {
	pool           *pgxpool.Pool
	idempotency    *idempotency.Service
	audit          *audit.Service
	idempotencyTTL time.Duration
}

type CreateRequest struct {
	Mutation    auth.Mutation
	Key         string
	DisplayName string
}

type Repository struct {
	ID          uuid.UUID `json:"id"`
	Key         string    `json:"key"`
	DisplayName string    `json:"displayName"`
	Type        string    `json:"type"`
	CreatedAt   time.Time `json:"createdAt"`
	Replayed    bool      `json:"-"`
}

type Cursor struct {
	Key string
}

type ListRequest struct {
	After *Cursor
	Limit int32
}

type Page struct {
	Items []Repository
	Next  *Cursor
}

func NewService(options Options) (*Service, error) {
	if options.Pool == nil {
		return nil, fmt.Errorf("repository service: pool is nil")
	}
	if options.Idempotency == nil {
		return nil, fmt.Errorf("repository service: idempotency service is nil")
	}
	if options.Audit == nil {
		return nil, fmt.Errorf("repository service: audit service is nil")
	}
	if options.IdempotencyTTL <= 0 {
		return nil, fmt.Errorf("repository service: idempotency TTL must be positive")
	}
	return &Service{
		pool:           options.Pool,
		idempotency:    options.Idempotency,
		audit:          options.Audit,
		idempotencyTTL: options.IdempotencyTTL,
	}, nil
}

func (s *Service) Create(ctx context.Context, request CreateRequest) (Repository, error) {
	if err := auth.Require(request.Mutation.Actor, auth.ScopeAdmin, uuid.Nil); err != nil {
		return Repository{}, err
	}
	if err := validateMutation(request.Mutation); err != nil {
		return Repository{}, err
	}
	idempotencyRequest := idempotency.Request{
		TokenID:           request.Mutation.Actor.TokenID,
		Method:            "POST",
		CanonicalResource: request.Mutation.CanonicalResource,
		Key:               request.Mutation.IdempotencyKey,
		Fingerprint:       request.Mutation.Fingerprint,
		TTL:               s.idempotencyTTL,
		RequestID:         request.Mutation.RequestID,
		ClassifyError:     classifyMutationError,
	}
	idempotencyRequest.OnTerminal = func(ctx context.Context, tx pgx.Tx, terminalErr error) error {
		response, ok := classifyMutationError(terminalErr)
		if !ok {
			return nil
		}
		_, err := s.audit.Record(ctx, tx, audit.Event{
			ActorTokenID: request.Mutation.Actor.TokenID,
			Action:       "repository.create",
			ResourceType: "repository",
			Outcome:      audit.OutcomeFailed,
			Code:         response.Code,
			RequestID:    request.Mutation.RequestID,
		})
		return err
	}
	result, err := s.idempotency.RunInTx(ctx, idempotencyRequest, func(ctx context.Context, tx pgx.Tx) (idempotency.Response, error) {
		created, err := db.New(tx).CreateRepository(ctx, db.CreateRepositoryParams{
			Key:         request.Key,
			DisplayName: request.DisplayName,
			CreatedBy:   request.Mutation.Actor.TokenID,
		})
		if err != nil {
			return idempotency.Response{}, mapDatabaseError("create repository", err)
		}
		value := repositoryFromRow(created)
		if _, err := s.audit.Record(ctx, tx, audit.Event{
			ActorTokenID: request.Mutation.Actor.TokenID,
			RepositoryID: created.ID,
			Action:       "repository.create",
			ResourceType: "repository",
			ResourceID:   created.ID.String(),
			Outcome:      audit.OutcomeSuccess,
			RequestID:    request.Mutation.RequestID,
			Details: map[string]any{
				"key":         created.Key,
				"displayName": created.DisplayName,
			},
		}); err != nil {
			return idempotency.Response{}, err
		}
		body, err := json.Marshal(value)
		if err != nil {
			return idempotency.Response{}, fmt.Errorf("encode repository response: %w", err)
		}
		return idempotency.Response{Status: 201, Body: body}, nil
	})
	if err != nil {
		return Repository{}, err
	}
	var created Repository
	if err := json.Unmarshal(result.Body, &created); err != nil {
		return Repository{}, fmt.Errorf("decode repository response: %w", err)
	}
	created.Replayed = result.Replayed
	return created, nil
}

func classifyMutationError(err error) (idempotency.ErrorResponse, bool) {
	switch {
	case errors.Is(err, auth.ErrForbidden):
		return idempotency.ErrorResponse{Status: 403, Title: "Forbidden", Code: "forbidden"}, true
	case errors.Is(err, ErrNotFound):
		return idempotency.ErrorResponse{Status: 404, Title: "Not Found", Code: "not-found"}, true
	case errors.Is(err, ErrInvalidRequest):
		return idempotency.ErrorResponse{Status: 400, Title: "Bad Request", Code: "invalid-request"}, true
	case errors.Is(err, ErrConflict):
		return idempotency.ErrorResponse{Status: 409, Title: "Conflict", Code: "conflict"}, true
	default:
		return idempotency.ErrorResponse{}, false
	}
}

func (s *Service) Get(ctx context.Context, actor auth.Actor, key string) (Repository, error) {
	if len(actor.Scopes) == 0 {
		return Repository{}, auth.ErrForbidden
	}
	row, err := db.New(s.pool).GetRepositoryByKey(ctx, key)
	if err != nil {
		return Repository{}, mapDatabaseError("get repository", err)
	}
	if !canSee(actor, row.ID) {
		return Repository{}, errors.Join(ErrNotFound, auth.ErrRepositoryDenied)
	}
	return repositoryFromRow(row), nil
}

func (s *Service) List(ctx context.Context, actor auth.Actor, request ListRequest) (Page, error) {
	if len(actor.Scopes) == 0 {
		return Page{}, auth.ErrForbidden
	}
	limit, err := normalizePageLimit(request.Limit)
	if err != nil {
		return Page{}, err
	}
	params := db.ListRepositoriesParams{PageLimit: limit + 1}
	if request.After != nil {
		if request.After.Key == "" {
			return Page{}, ErrInvalidRequest
		}
		params.AfterKey = request.After.Key
	}
	if !actor.Scopes.Has(auth.ScopeAdmin) {
		if len(actor.RepositoryIDs) == 0 {
			return Page{Items: []Repository{}}, nil
		}
		params.RepositoryIds = make([]uuid.UUID, 0, len(actor.RepositoryIDs))
		for repositoryID := range actor.RepositoryIDs {
			params.RepositoryIds = append(params.RepositoryIds, repositoryID)
		}
	}
	rows, err := db.New(s.pool).ListRepositories(ctx, params)
	if err != nil {
		return Page{}, fmt.Errorf("list repositories: %w", err)
	}
	pageRows := rows[:min(len(rows), int(limit))]
	page := Page{Items: make([]Repository, 0, len(pageRows))}
	for _, row := range pageRows {
		page.Items = append(page.Items, repositoryFromRow(row))
	}
	if len(rows) > int(limit) {
		page.Next = &Cursor{Key: page.Items[len(page.Items)-1].Key}
	}
	return page, nil
}

func repositoryFromRow(row db.Repository) Repository {
	return Repository{
		ID:          row.ID,
		Key:         row.Key,
		DisplayName: row.DisplayName,
		Type:        row.RepositoryType,
		CreatedAt:   row.CreatedAt.Time.UTC(),
	}
}

func canSee(actor auth.Actor, repositoryID uuid.UUID) bool {
	if actor.Scopes.Has(auth.ScopeAdmin) {
		return true
	}
	_, allowed := actor.RepositoryIDs[repositoryID]
	return allowed
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

func normalizePageLimit(value int32) (int32, error) {
	if value == 0 {
		return defaultPageLimit, nil
	}
	if value < 1 || value > maxPageLimit {
		return 0, ErrInvalidRequest
	}
	return value, nil
}

func mapDatabaseError(operation string, err error) error {
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
