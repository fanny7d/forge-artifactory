package product

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
	"unicode/utf8"

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
	CLIRepositoryKey       = "cli-releases"
	defaultPageLimit int32 = 50
	maxPageLimit     int32 = 200
)

var (
	ErrNotFound       = errors.New("product not found")
	ErrConflict       = errors.New("product conflict")
	ErrInvalidRequest = errors.New("invalid product request")

	slugPattern        = regexp.MustCompile(`^[a-z][a-z0-9._-]{1,63}$`)
	commandNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
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
	Slug        string
	DisplayName string
	Description string
	CommandName string
}

type RotateInstallKeyRequest struct {
	Mutation auth.Mutation
	Slug     string
}

type Platform struct {
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Variant  string `json:"variant"`
	Strategy string `json:"strategy"`
	Format   string `json:"format"`
}

type Product struct {
	ID                       uuid.UUID  `json:"id"`
	Slug                     string     `json:"slug"`
	PackageID                uuid.UUID  `json:"packageId"`
	RepositoryID             uuid.UUID  `json:"repositoryId"`
	RepositoryKey            string     `json:"repository"`
	PackageName              string     `json:"packageName"`
	DisplayName              string     `json:"displayName"`
	Description              string     `json:"description"`
	CommandName              string     `json:"commandName"`
	InstallKey               uuid.UUID  `json:"installKey"`
	CurrentStableVersion     *string    `json:"currentStableVersion,omitempty"`
	CurrentStablePublishedAt *time.Time `json:"publishedAt,omitempty"`
	Platforms                []Platform `json:"platforms"`
	CreatedAt                time.Time  `json:"createdAt"`
	UpdatedAt                time.Time  `json:"updatedAt"`
	Replayed                 bool       `json:"-"`
}

type Cursor struct {
	Slug      string
	ID        uuid.UUID
	CreatedAt time.Time
}

type ListRequest struct {
	After *Cursor
	Limit int32
}

type Page struct {
	Items []Product
	Next  *Cursor
}

type productFields struct {
	ID                       uuid.UUID
	Slug                     string
	PackageID                uuid.UUID
	RepositoryID             uuid.UUID
	RepositoryKey            string
	PackageName              string
	DisplayName              string
	Description              string
	CommandName              string
	InstallKey               uuid.UUID
	CurrentStableVersion     *string
	CurrentStablePublishedAt *time.Time
	PlatformsJSON            string
	CreatedAt                time.Time
	UpdatedAt                time.Time
}

func NewService(options Options) (*Service, error) {
	if options.Pool == nil {
		return nil, fmt.Errorf("product service: pool is nil")
	}
	if options.Idempotency == nil {
		return nil, fmt.Errorf("product service: idempotency service is nil")
	}
	if options.Audit == nil {
		return nil, fmt.Errorf("product service: audit service is nil")
	}
	if options.IdempotencyTTL <= 0 {
		return nil, fmt.Errorf("product service: idempotency TTL must be positive")
	}
	return &Service{
		pool:           options.Pool,
		idempotency:    options.Idempotency,
		audit:          options.Audit,
		idempotencyTTL: options.IdempotencyTTL,
	}, nil
}

func (s *Service) Create(ctx context.Context, request CreateRequest) (Product, error) {
	if err := auth.Require(request.Mutation.Actor, auth.ScopeAdmin, uuid.Nil); err != nil {
		return Product{}, err
	}
	if err := validateMutation(request.Mutation); err != nil {
		return Product{}, err
	}
	if err := validateProductFields(request); err != nil {
		return Product{}, err
	}

	var terminalRepositoryID uuid.UUID
	idempotencyRequest := s.mutationRequest(
		request.Mutation,
		"product.create",
		&terminalRepositoryID,
	)
	result, err := s.idempotency.RunInTx(ctx, idempotencyRequest, func(ctx context.Context, tx pgx.Tx) (idempotency.Response, error) {
		queries := db.New(tx)
		repository, err := queries.EnsureCLIRepository(ctx, request.Mutation.Actor.TokenID)
		if err != nil {
			return idempotency.Response{}, mapDatabaseError("ensure CLI repository", err)
		}
		terminalRepositoryID = repository.ID

		productPackage, err := queries.EnsureProductPackage(ctx, db.EnsureProductPackageParams{
			RepositoryID: repository.ID,
			Name:         request.Slug,
			CreatedBy:    request.Mutation.Actor.TokenID,
		})
		if err != nil {
			return idempotency.Response{}, mapDatabaseError("ensure product package", err)
		}
		for _, channelName := range []string{"candidate", "stable"} {
			if _, err := queries.EnsureProductChannel(ctx, db.EnsureProductChannelParams{
				PackageID: productPackage.ID,
				Name:      channelName,
			}); err != nil {
				return idempotency.Response{}, mapDatabaseError("ensure product channel", err)
			}
		}

		created, err := queries.CreateProduct(ctx, db.CreateProductParams{
			Slug:        request.Slug,
			PackageID:   productPackage.ID,
			DisplayName: request.DisplayName,
			Description: request.Description,
			CommandName: request.CommandName,
			CreatedBy:   request.Mutation.Actor.TokenID,
		})
		if err != nil {
			return idempotency.Response{}, mapDatabaseError("create product", err)
		}
		value, err := productFromFields(productFields{
			ID:            created.ID,
			Slug:          created.Slug,
			PackageID:     created.PackageID,
			RepositoryID:  repository.ID,
			RepositoryKey: repository.Key,
			PackageName:   productPackage.Name,
			DisplayName:   created.DisplayName,
			Description:   created.Description,
			CommandName:   created.CommandName,
			InstallKey:    created.InstallKey,
			PlatformsJSON: "[]",
			CreatedAt:     created.CreatedAt.Time,
			UpdatedAt:     created.UpdatedAt.Time,
		})
		if err != nil {
			return idempotency.Response{}, err
		}
		if _, err := s.audit.Record(ctx, tx, audit.Event{
			ActorTokenID: request.Mutation.Actor.TokenID,
			RepositoryID: repository.ID,
			Action:       "product.create",
			ResourceType: "product",
			ResourceID:   created.ID.String(),
			Outcome:      audit.OutcomeSuccess,
			RequestID:    request.Mutation.RequestID,
			Details: map[string]any{
				"slug":        created.Slug,
				"packageId":   productPackage.ID.String(),
				"repository":  repository.Key,
				"displayName": created.DisplayName,
				"commandName": created.CommandName,
			},
		}); err != nil {
			return idempotency.Response{}, err
		}
		body, err := json.Marshal(value)
		if err != nil {
			return idempotency.Response{}, fmt.Errorf("encode product response: %w", err)
		}
		return idempotency.Response{Status: 201, Body: body, Encrypt: true}, nil
	})
	if err != nil {
		return Product{}, err
	}

	var created Product
	if err := json.Unmarshal(result.Body, &created); err != nil {
		return Product{}, fmt.Errorf("decode product response: %w", err)
	}
	created.Replayed = result.Replayed
	return created, nil
}

func (s *Service) RotateInstallKey(ctx context.Context, request RotateInstallKeyRequest) (Product, error) {
	if err := auth.Require(request.Mutation.Actor, auth.ScopeAdmin, uuid.Nil); err != nil {
		return Product{}, err
	}
	if err := validateMutation(request.Mutation); err != nil {
		return Product{}, err
	}
	if !slugPattern.MatchString(request.Slug) {
		return Product{}, ErrInvalidRequest
	}

	var terminalRepositoryID uuid.UUID
	idempotencyRequest := s.mutationRequest(
		request.Mutation,
		"product.install-key.rotate",
		&terminalRepositoryID,
	)
	result, err := s.idempotency.RunInTx(ctx, idempotencyRequest, func(ctx context.Context, tx pgx.Tx) (idempotency.Response, error) {
		queries := db.New(tx)
		if _, err := queries.RotateProductInstallKey(ctx, request.Slug); err != nil {
			return idempotency.Response{}, mapDatabaseError("rotate product install key", err)
		}
		row, err := queries.GetVisibleProductBySlug(ctx, db.GetVisibleProductBySlugParams{
			Slug:          request.Slug,
			IncludeAll:    true,
			RepositoryIds: []uuid.UUID{},
		})
		if err != nil {
			return idempotency.Response{}, mapDatabaseError("get rotated product", err)
		}
		terminalRepositoryID = row.RepositoryID
		value, err := productFromVisibleRow(row)
		if err != nil {
			return idempotency.Response{}, err
		}
		if _, err := s.audit.Record(ctx, tx, audit.Event{
			ActorTokenID: request.Mutation.Actor.TokenID,
			RepositoryID: row.RepositoryID,
			Action:       "product.install-key.rotate",
			ResourceType: "product",
			ResourceID:   row.ID.String(),
			Outcome:      audit.OutcomeSuccess,
			RequestID:    request.Mutation.RequestID,
			Details: map[string]any{
				"slug": request.Slug,
			},
		}); err != nil {
			return idempotency.Response{}, err
		}
		body, err := json.Marshal(value)
		if err != nil {
			return idempotency.Response{}, fmt.Errorf("encode rotated product response: %w", err)
		}
		return idempotency.Response{Status: 200, Body: body, Encrypt: true}, nil
	})
	if err != nil {
		return Product{}, err
	}

	var rotated Product
	if err := json.Unmarshal(result.Body, &rotated); err != nil {
		return Product{}, fmt.Errorf("decode rotated product response: %w", err)
	}
	rotated.Replayed = result.Replayed
	return rotated, nil
}

func (s *Service) Get(ctx context.Context, actor auth.Actor, slug string) (Product, error) {
	if err := auth.Require(actor, auth.ScopeAdmin, uuid.Nil); err != nil {
		return Product{}, err
	}
	if !slugPattern.MatchString(slug) {
		return Product{}, ErrInvalidRequest
	}
	row, err := db.New(s.pool).GetVisibleProductBySlug(ctx, db.GetVisibleProductBySlugParams{
		Slug:          slug,
		IncludeAll:    true,
		RepositoryIds: []uuid.UUID{},
	})
	if err != nil {
		return Product{}, mapDatabaseError("get product", err)
	}
	return productFromVisibleRow(row)
}

func (s *Service) GetByInstallKey(ctx context.Context, installKey uuid.UUID) (Product, error) {
	if installKey == uuid.Nil {
		return Product{}, ErrInvalidRequest
	}
	row, err := db.New(s.pool).GetProductByInstallKey(ctx, installKey)
	if err != nil {
		return Product{}, mapDatabaseError("get product by install key", err)
	}
	return productFromInstallKeyRow(row)
}

func (s *Service) GetByPackageID(ctx context.Context, packageID uuid.UUID) (Product, error) {
	if packageID == uuid.Nil {
		return Product{}, ErrInvalidRequest
	}
	queries := db.New(s.pool)
	productRow, err := queries.GetProductByPackageID(ctx, packageID)
	if err != nil {
		return Product{}, mapDatabaseError("get product by package ID", err)
	}
	row, err := queries.GetVisibleProductBySlug(ctx, db.GetVisibleProductBySlugParams{
		Slug:          productRow.Slug,
		IncludeAll:    true,
		RepositoryIds: []uuid.UUID{},
	})
	if err != nil {
		return Product{}, mapDatabaseError("get product details by package ID", err)
	}
	return productFromVisibleRow(row)
}

func (s *Service) List(ctx context.Context, actor auth.Actor, request ListRequest) (Page, error) {
	if err := auth.Require(actor, auth.ScopeAdmin, uuid.Nil); err != nil {
		return Page{}, err
	}
	limit, err := normalizePageLimit(request.Limit)
	if err != nil {
		return Page{}, err
	}
	params := db.ListVisibleProductsParams{
		IncludeAll:    true,
		RepositoryIds: []uuid.UUID{},
		PageLimit:     limit + 1,
	}
	if request.After != nil {
		if !slugPattern.MatchString(request.After.Slug) ||
			request.After.ID == uuid.Nil ||
			request.After.CreatedAt.IsZero() {
			return Page{}, ErrInvalidRequest
		}
		params.AfterSlug = request.After.Slug
	}
	rows, err := db.New(s.pool).ListVisibleProducts(ctx, params)
	if err != nil {
		return Page{}, fmt.Errorf("list products: %w", err)
	}

	pageRows := rows[:min(len(rows), int(limit))]
	page := Page{Items: make([]Product, 0, len(pageRows))}
	for _, row := range pageRows {
		item, err := productFromListRow(row)
		if err != nil {
			return Page{}, err
		}
		page.Items = append(page.Items, item)
	}
	if len(rows) > int(limit) {
		last := page.Items[len(page.Items)-1]
		page.Next = &Cursor{
			Slug:      last.Slug,
			ID:        last.ID,
			CreatedAt: last.CreatedAt,
		}
	}
	return page, nil
}

func (s *Service) mutationRequest(mutation auth.Mutation, action string, repositoryID *uuid.UUID) idempotency.Request {
	method := mutation.Method
	if method == "" {
		method = "POST"
	}
	request := idempotency.Request{
		TokenID:           mutation.Actor.TokenID,
		Method:            method,
		CanonicalResource: mutation.CanonicalResource,
		Key:               mutation.IdempotencyKey,
		Fingerprint:       mutation.Fingerprint,
		TTL:               s.idempotencyTTL,
		RequestID:         mutation.RequestID,
		ClassifyError:     classifyMutationError,
	}
	request.OnTerminal = func(ctx context.Context, tx pgx.Tx, terminalErr error) error {
		response, ok := classifyMutationError(terminalErr)
		if !ok {
			return nil
		}
		outcome := audit.OutcomeFailed
		if errors.Is(terminalErr, auth.ErrForbidden) {
			outcome = audit.OutcomeDenied
		}
		_, err := s.audit.Record(ctx, tx, audit.Event{
			ActorTokenID: mutation.Actor.TokenID,
			RepositoryID: *repositoryID,
			Action:       action,
			ResourceType: "product",
			Outcome:      outcome,
			Code:         response.Code,
			RequestID:    mutation.RequestID,
		})
		return err
	}
	return request
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

func productFromVisibleRow(row db.GetVisibleProductBySlugRow) (Product, error) {
	return productFromFields(productFields{
		ID:                       row.ID,
		Slug:                     row.Slug,
		PackageID:                row.PackageID,
		RepositoryID:             row.RepositoryID,
		RepositoryKey:            row.RepositoryKey,
		PackageName:              row.PackageName,
		DisplayName:              row.DisplayName,
		Description:              row.Description,
		CommandName:              row.CommandName,
		InstallKey:               row.InstallKey,
		CurrentStableVersion:     row.CurrentStableVersion,
		CurrentStablePublishedAt: nullableTime(row.PublishedAt),
		PlatformsJSON:            row.PlatformsJson,
		CreatedAt:                row.CreatedAt.Time,
		UpdatedAt:                row.UpdatedAt.Time,
	})
}

func productFromInstallKeyRow(row db.GetProductByInstallKeyRow) (Product, error) {
	return productFromFields(productFields{
		ID:                       row.ID,
		Slug:                     row.Slug,
		PackageID:                row.PackageID,
		RepositoryID:             row.RepositoryID,
		RepositoryKey:            row.RepositoryKey,
		PackageName:              row.PackageName,
		DisplayName:              row.DisplayName,
		Description:              row.Description,
		CommandName:              row.CommandName,
		InstallKey:               row.InstallKey,
		CurrentStableVersion:     row.CurrentStableVersion,
		CurrentStablePublishedAt: nullableTime(row.PublishedAt),
		PlatformsJSON:            row.PlatformsJson,
		CreatedAt:                row.CreatedAt.Time,
		UpdatedAt:                row.UpdatedAt.Time,
	})
}

func productFromListRow(row db.ListVisibleProductsRow) (Product, error) {
	return productFromFields(productFields{
		ID:                       row.ID,
		Slug:                     row.Slug,
		PackageID:                row.PackageID,
		RepositoryID:             row.RepositoryID,
		RepositoryKey:            row.RepositoryKey,
		PackageName:              row.PackageName,
		DisplayName:              row.DisplayName,
		Description:              row.Description,
		CommandName:              row.CommandName,
		InstallKey:               row.InstallKey,
		CurrentStableVersion:     row.CurrentStableVersion,
		CurrentStablePublishedAt: nullableTime(row.PublishedAt),
		PlatformsJSON:            row.PlatformsJson,
		CreatedAt:                row.CreatedAt.Time,
		UpdatedAt:                row.UpdatedAt.Time,
	})
}

func productFromFields(fields productFields) (Product, error) {
	platforms := []Platform{}
	if err := json.Unmarshal([]byte(fields.PlatformsJSON), &platforms); err != nil {
		return Product{}, fmt.Errorf("decode product %s platforms: %w", fields.ID, err)
	}
	return Product{
		ID:                       fields.ID,
		Slug:                     fields.Slug,
		PackageID:                fields.PackageID,
		RepositoryID:             fields.RepositoryID,
		RepositoryKey:            fields.RepositoryKey,
		PackageName:              fields.PackageName,
		DisplayName:              fields.DisplayName,
		Description:              fields.Description,
		CommandName:              fields.CommandName,
		InstallKey:               fields.InstallKey,
		CurrentStableVersion:     cloneString(fields.CurrentStableVersion),
		CurrentStablePublishedAt: cloneTime(fields.CurrentStablePublishedAt),
		Platforms:                platforms,
		CreatedAt:                fields.CreatedAt.UTC(),
		UpdatedAt:                fields.UpdatedAt.UTC(),
	}, nil
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

func validateProductFields(request CreateRequest) error {
	displayNameLength := utf8.RuneCountInString(request.DisplayName)
	descriptionLength := utf8.RuneCountInString(request.Description)
	if !slugPattern.MatchString(request.Slug) ||
		displayNameLength < 1 ||
		displayNameLength > 128 ||
		descriptionLength > 2048 ||
		!commandNamePattern.MatchString(request.CommandName) {
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

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func nullableTime(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	normalized := value.Time.UTC()
	return &normalized
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := value.UTC()
	return &copy
}
