package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"superfan.myasustor.com/fanchao/artifact-repository/internal/audit"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/clock"
	db "superfan.myasustor.com/fanchao/artifact-repository/internal/database/sqlc"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/id"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/idempotency"
)

const (
	defaultPageLimit int32 = 50
	maxPageLimit     int32 = 200
)

var (
	ErrNotFound       = errors.New("authentication resource not found")
	ErrConflict       = errors.New("authentication resource conflict")
	ErrInvalidRequest = errors.New("invalid authentication request")
)

type ServiceOptions struct {
	Pool           *pgxpool.Pool
	Idempotency    *idempotency.Service
	Audit          *audit.Service
	Pepper         []byte
	Random         io.Reader
	IDs            id.Generator
	Clock          clock.Clock
	IdempotencyTTL time.Duration
}

type Service struct {
	pool           *pgxpool.Pool
	idempotency    *idempotency.Service
	audit          *audit.Service
	pepper         []byte
	random         io.Reader
	ids            id.Generator
	clock          clock.Clock
	idempotencyTTL time.Duration
}

type Mutation struct {
	Actor             Actor
	Method            string
	RequestID         string
	IdempotencyKey    string
	Fingerprint       []byte
	CanonicalResource string
}

type CreateServiceAccountRequest struct {
	Mutation Mutation
	Name     string
}

type ServiceAccountResult struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
	Replayed  bool      `json:"-"`
}

type Cursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

type ListRequest struct {
	After *Cursor
	Limit int32
}

type ServiceAccountPage struct {
	Items []ServiceAccountResult
	Next  *Cursor
}

type IssueTokenRequest struct {
	Mutation         Mutation
	ServiceAccountID uuid.UUID
	Scopes           []Scope
	Repositories     []string
	ExpiresAt        time.Time
}

type Token struct {
	ID               uuid.UUID  `json:"id"`
	ServiceAccountID uuid.UUID  `json:"serviceAccountId"`
	Scopes           []Scope    `json:"scopes"`
	Repositories     []string   `json:"repositories"`
	ExpiresAt        time.Time  `json:"expiresAt"`
	Revoked          bool       `json:"revoked"`
	LastUsedAt       *time.Time `json:"lastUsedAt,omitempty"`
	CreatedAt        time.Time  `json:"createdAt"`
}

type IssuedTokenDetails struct {
	Token
	Secret   string `json:"secret"`
	Replayed bool   `json:"-"`
}

type TokenPage struct {
	Items []Token
	Next  *Cursor
}

type RevokeTokenRequest struct {
	Mutation Mutation
	TokenID  uuid.UUID
}

type RevokeTokenResult struct {
	Replayed bool
}

type SigningKey struct {
	KeyID       string
	Algorithm   string
	PublicKey   []byte
	Fingerprint string
	Active      bool
	CreatedAt   time.Time
}

func NewService(options ServiceOptions) (*Service, error) {
	if options.Pool == nil {
		return nil, fmt.Errorf("auth service: pool is nil")
	}
	if options.Idempotency == nil {
		return nil, fmt.Errorf("auth service: idempotency service is nil")
	}
	if options.Audit == nil {
		return nil, fmt.Errorf("auth service: audit service is nil")
	}
	if len(options.Pepper) != 32 {
		return nil, fmt.Errorf("auth service: pepper must be 32 bytes")
	}
	if options.Random == nil {
		return nil, fmt.Errorf("auth service: random reader is nil")
	}
	if options.IDs == nil {
		return nil, fmt.Errorf("auth service: ID generator is nil")
	}
	if options.Clock == nil {
		return nil, fmt.Errorf("auth service: clock is nil")
	}
	if options.IdempotencyTTL <= 0 {
		return nil, fmt.Errorf("auth service: idempotency TTL must be positive")
	}
	return &Service{
		pool:           options.Pool,
		idempotency:    options.Idempotency,
		audit:          options.Audit,
		pepper:         append([]byte(nil), options.Pepper...),
		random:         options.Random,
		ids:            options.IDs,
		clock:          options.Clock,
		idempotencyTTL: options.IdempotencyTTL,
	}, nil
}

func (s *Service) CreateServiceAccount(ctx context.Context, request CreateServiceAccountRequest) (ServiceAccountResult, error) {
	if err := s.validateAdminMutation(request.Mutation); err != nil {
		return ServiceAccountResult{}, err
	}
	result, err := s.idempotency.RunInTx(ctx, s.idempotencyRequest(request.Mutation, "service-account.create", "service_account"), func(ctx context.Context, tx pgx.Tx) (idempotency.Response, error) {
		created, err := db.New(tx).CreateServiceAccount(ctx, request.Name)
		if err != nil {
			return idempotency.Response{}, mapDatabaseError("create service account", err)
		}
		value := serviceAccountFromRow(created)
		if _, err := s.audit.Record(ctx, tx, audit.Event{
			ActorTokenID: request.Mutation.Actor.TokenID,
			Action:       "service-account.create",
			ResourceType: "service_account",
			ResourceID:   created.ID.String(),
			Outcome:      audit.OutcomeSuccess,
			RequestID:    request.Mutation.RequestID,
			Details:      map[string]any{"name": created.Name},
		}); err != nil {
			return idempotency.Response{}, err
		}
		body, err := json.Marshal(value)
		if err != nil {
			return idempotency.Response{}, fmt.Errorf("encode service account response: %w", err)
		}
		return idempotency.Response{Status: 201, Body: body}, nil
	})
	if err != nil {
		return ServiceAccountResult{}, err
	}
	var created ServiceAccountResult
	if err := json.Unmarshal(result.Body, &created); err != nil {
		return ServiceAccountResult{}, fmt.Errorf("decode service account response: %w", err)
	}
	created.Replayed = result.Replayed
	return created, nil
}

func (s *Service) GetServiceAccount(ctx context.Context, actor Actor, serviceAccountID uuid.UUID) (ServiceAccountResult, error) {
	if err := Require(actor, ScopeAdmin, uuid.Nil); err != nil {
		return ServiceAccountResult{}, err
	}
	row, err := db.New(s.pool).GetServiceAccount(ctx, serviceAccountID)
	if err != nil {
		return ServiceAccountResult{}, mapDatabaseError("get service account", err)
	}
	return serviceAccountFromRow(row), nil
}

func (s *Service) ListServiceAccounts(ctx context.Context, actor Actor, request ListRequest) (ServiceAccountPage, error) {
	if err := Require(actor, ScopeAdmin, uuid.Nil); err != nil {
		return ServiceAccountPage{}, err
	}
	limit, err := normalizePageLimit(request.Limit)
	if err != nil {
		return ServiceAccountPage{}, err
	}
	params := db.ListServiceAccountsParams{AfterID: uuid.Nil, PageLimit: limit + 1}
	if request.After != nil {
		if request.After.ID == uuid.Nil || request.After.CreatedAt.IsZero() {
			return ServiceAccountPage{}, ErrInvalidRequest
		}
		params.AfterCreatedAt = pgtype.Timestamptz{Time: request.After.CreatedAt, Valid: true}
		params.AfterID = request.After.ID
	}
	rows, err := db.New(s.pool).ListServiceAccounts(ctx, params)
	if err != nil {
		return ServiceAccountPage{}, fmt.Errorf("list service accounts: %w", err)
	}
	page := ServiceAccountPage{Items: make([]ServiceAccountResult, 0, min(len(rows), int(limit)))}
	for _, row := range rows[:min(len(rows), int(limit))] {
		page.Items = append(page.Items, serviceAccountFromRow(row))
	}
	if len(rows) > int(limit) {
		last := page.Items[len(page.Items)-1]
		page.Next = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return page, nil
}

func (s *Service) IssueToken(ctx context.Context, request IssueTokenRequest) (IssuedTokenDetails, error) {
	if err := s.validateAdminMutation(request.Mutation); err != nil {
		return IssuedTokenDetails{}, err
	}
	if request.ServiceAccountID == uuid.Nil || !request.ExpiresAt.After(s.clock.Now()) || len(request.Scopes) == 0 {
		return IssuedTokenDetails{}, ErrInvalidRequest
	}
	if err := validateScopes(request.Scopes); err != nil {
		return IssuedTokenDetails{}, err
	}
	if hasDuplicates(request.Repositories) {
		return IssuedTokenDetails{}, ErrInvalidRequest
	}

	result, err := s.idempotency.RunInTx(ctx, s.idempotencyRequest(request.Mutation, "token.create", "api_token"), func(ctx context.Context, tx pgx.Tx) (idempotency.Response, error) {
		queries := db.New(tx)
		if _, err := queries.GetServiceAccount(ctx, request.ServiceAccountID); err != nil {
			return idempotency.Response{}, mapDatabaseError("get service account", err)
		}
		repositoryIDs := make([]uuid.UUID, 0, len(request.Repositories))
		for _, key := range request.Repositories {
			repository, err := queries.GetRepositoryByKey(ctx, key)
			if err != nil {
				return idempotency.Response{}, mapDatabaseError("get token repository", err)
			}
			repositoryIDs = append(repositoryIDs, repository.ID)
		}

		tokenID := s.ids.New()
		issuedSecret, err := IssueToken(s.random, tokenID, s.pepper)
		if err != nil {
			return idempotency.Response{}, err
		}
		row, err := queries.CreateAPIToken(ctx, db.CreateAPITokenParams{
			ID:               tokenID,
			ServiceAccountID: request.ServiceAccountID,
			SecretHmac:       issuedSecret.SecretHMAC,
			Scopes:           scopeStrings(request.Scopes),
			RepositoryIds:    repositoryIDs,
			ExpiresAt:        pgtype.Timestamptz{Time: request.ExpiresAt.UTC(), Valid: true},
		})
		if err != nil {
			return idempotency.Response{}, mapDatabaseError("create API token", err)
		}
		value := IssuedTokenDetails{
			Token:  tokenFromRow(row, append([]string(nil), request.Repositories...)),
			Secret: issuedSecret.Bearer,
		}
		if _, err := s.audit.Record(ctx, tx, audit.Event{
			ActorTokenID: request.Mutation.Actor.TokenID,
			Action:       "token.create",
			ResourceType: "api_token",
			ResourceID:   row.ID.String(),
			Outcome:      audit.OutcomeSuccess,
			RequestID:    request.Mutation.RequestID,
			Details: map[string]any{
				"serviceAccountId": request.ServiceAccountID.String(),
				"scopes":           scopeStrings(request.Scopes),
				"repositories":     request.Repositories,
			},
		}); err != nil {
			return idempotency.Response{}, err
		}
		body, err := json.Marshal(value)
		if err != nil {
			return idempotency.Response{}, fmt.Errorf("encode issued token response: %w", err)
		}
		return idempotency.Response{Status: 201, Body: body, Encrypt: true}, nil
	})
	if err != nil {
		return IssuedTokenDetails{}, err
	}
	var issued IssuedTokenDetails
	if err := json.Unmarshal(result.Body, &issued); err != nil {
		return IssuedTokenDetails{}, fmt.Errorf("decode issued token response: %w", err)
	}
	issued.Replayed = result.Replayed
	return issued, nil
}

func (s *Service) ListTokens(ctx context.Context, actor Actor, serviceAccountID uuid.UUID, request ListRequest) (TokenPage, error) {
	if err := Require(actor, ScopeAdmin, uuid.Nil); err != nil {
		return TokenPage{}, err
	}
	limit, err := normalizePageLimit(request.Limit)
	if err != nil {
		return TokenPage{}, err
	}
	queries := db.New(s.pool)
	if _, err := queries.GetServiceAccount(ctx, serviceAccountID); err != nil {
		return TokenPage{}, mapDatabaseError("get service account", err)
	}
	params := db.ListAPITokensParams{
		ServiceAccountID: serviceAccountID,
		AfterID:          uuid.Nil,
		PageLimit:        limit + 1,
	}
	if request.After != nil {
		if request.After.ID == uuid.Nil || request.After.CreatedAt.IsZero() {
			return TokenPage{}, ErrInvalidRequest
		}
		params.AfterCreatedAt = pgtype.Timestamptz{Time: request.After.CreatedAt, Valid: true}
		params.AfterID = request.After.ID
	}
	rows, err := queries.ListAPITokens(ctx, params)
	if err != nil {
		return TokenPage{}, fmt.Errorf("list API tokens: %w", err)
	}
	pageRows := rows[:min(len(rows), int(limit))]
	page := TokenPage{Items: make([]Token, 0, len(pageRows))}
	for _, row := range pageRows {
		repositories := make([]string, 0, len(row.RepositoryIds))
		for _, repositoryID := range row.RepositoryIds {
			repository, err := queries.GetRepositoryByID(ctx, repositoryID)
			if err != nil {
				return TokenPage{}, fmt.Errorf("get token repository %s: %w", repositoryID, err)
			}
			repositories = append(repositories, repository.Key)
		}
		page.Items = append(page.Items, tokenFromRow(row, repositories))
	}
	if len(rows) > int(limit) {
		last := page.Items[len(page.Items)-1]
		page.Next = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return page, nil
}

func (s *Service) RevokeToken(ctx context.Context, request RevokeTokenRequest) (RevokeTokenResult, error) {
	if err := s.validateAdminMutation(request.Mutation); err != nil {
		return RevokeTokenResult{}, err
	}
	if request.TokenID == uuid.Nil {
		return RevokeTokenResult{}, ErrInvalidRequest
	}
	result, err := s.idempotency.RunInTx(ctx, s.idempotencyRequest(request.Mutation, "token.revoke", "api_token"), func(ctx context.Context, tx pgx.Tx) (idempotency.Response, error) {
		queries := db.New(tx)
		if _, err := queries.GetAPITokenForAuthentication(ctx, request.TokenID); err != nil {
			return idempotency.Response{}, mapDatabaseError("get API token", err)
		}
		if _, err := queries.RevokeAPIToken(ctx, db.RevokeAPITokenParams{
			ID:        request.TokenID,
			RevokedAt: pgtype.Timestamptz{Time: s.clock.Now().UTC(), Valid: true},
		}); err != nil {
			return idempotency.Response{}, fmt.Errorf("revoke API token: %w", err)
		}
		if _, err := s.audit.Record(ctx, tx, audit.Event{
			ActorTokenID: request.Mutation.Actor.TokenID,
			Action:       "token.revoke",
			ResourceType: "api_token",
			ResourceID:   request.TokenID.String(),
			Outcome:      audit.OutcomeSuccess,
			RequestID:    request.Mutation.RequestID,
		}); err != nil {
			return idempotency.Response{}, err
		}
		return idempotency.Response{Status: 204, Body: []byte{}}, nil
	})
	if err != nil {
		return RevokeTokenResult{}, err
	}
	return RevokeTokenResult{Replayed: result.Replayed}, nil
}

func (s *Service) Authenticate(ctx context.Context, bearer string) (Actor, error) {
	tokenID, secret, err := ParseBearer(bearer)
	if err != nil {
		return Actor{}, ErrInvalidToken
	}
	queries := db.New(s.pool)
	row, err := queries.GetAPITokenForAuthentication(ctx, tokenID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Actor{}, ErrInvalidToken
	}
	if err != nil {
		return Actor{}, fmt.Errorf("authenticate API token: %w", err)
	}
	now := s.clock.Now().UTC()
	if row.RevokedAt.Valid || !row.ExpiresAt.Time.After(now) || !VerifySecret(s.pepper, tokenID, secret, row.SecretHmac) {
		return Actor{}, ErrInvalidToken
	}
	if err := queries.TouchAPIToken(ctx, db.TouchAPITokenParams{
		ID:         tokenID,
		LastUsedAt: pgtype.Timestamptz{Time: now, Valid: true},
	}); err != nil {
		return Actor{}, fmt.Errorf("touch API token: %w", err)
	}
	actor := Actor{
		TokenID:            row.ID,
		ServiceAccountID:   row.ServiceAccountID,
		ServiceAccountName: row.ServiceAccountName,
		Scopes:             make(ScopeSet, len(row.Scopes)),
		RepositoryIDs:      make(map[uuid.UUID]struct{}, len(row.RepositoryIds)),
	}
	for _, scope := range row.Scopes {
		actor.Scopes[Scope(scope)] = struct{}{}
	}
	for _, repositoryID := range row.RepositoryIds {
		actor.RepositoryIDs[repositoryID] = struct{}{}
	}
	return actor, nil
}

func (s *Service) GetSigningKey(ctx context.Context, actor Actor, keyID string) (SigningKey, error) {
	if actor.TokenID == uuid.Nil || len(actor.Scopes) == 0 {
		return SigningKey{}, ErrForbidden
	}
	row, err := db.New(s.pool).GetSigningKey(ctx, keyID)
	if err != nil {
		return SigningKey{}, mapDatabaseError("get signing key", err)
	}
	return SigningKey{
		KeyID:       row.KeyID,
		Algorithm:   row.Algorithm,
		PublicKey:   append([]byte(nil), row.PublicKey...),
		Fingerprint: row.Fingerprint,
		Active:      row.Active,
		CreatedAt:   row.CreatedAt.Time.UTC(),
	}, nil
}

func (s *Service) validateAdminMutation(mutation Mutation) error {
	if err := Require(mutation.Actor, ScopeAdmin, uuid.Nil); err != nil {
		return err
	}
	if mutation.Actor.TokenID == uuid.Nil || mutation.RequestID == "" || mutation.CanonicalResource == "" {
		return ErrInvalidRequest
	}
	if mutation.IdempotencyKey != "" && len(mutation.Fingerprint) != 32 {
		return ErrInvalidRequest
	}
	return nil
}

func (s *Service) idempotencyRequest(mutation Mutation, action, resourceType string) idempotency.Request {
	request := idempotency.Request{
		TokenID:           mutation.Actor.TokenID,
		Method:            mutationMethod(mutation),
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
		if errors.Is(terminalErr, ErrForbidden) {
			outcome = audit.OutcomeDenied
		}
		_, err := s.audit.Record(ctx, tx, audit.Event{
			ActorTokenID: mutation.Actor.TokenID,
			Action:       action,
			ResourceType: resourceType,
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
	case errors.Is(err, ErrForbidden):
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

func mutationMethod(mutation Mutation) string {
	if mutation.Method == "" {
		return "POST"
	}
	return mutation.Method
}

func serviceAccountFromRow(row db.ServiceAccount) ServiceAccountResult {
	return ServiceAccountResult{
		ID:        row.ID,
		Name:      row.Name,
		CreatedAt: row.CreatedAt.Time.UTC(),
	}
}

func tokenFromRow(row db.ApiToken, repositories []string) Token {
	var lastUsedAt *time.Time
	if row.LastUsedAt.Valid {
		value := row.LastUsedAt.Time.UTC()
		lastUsedAt = &value
	}
	return Token{
		ID:               row.ID,
		ServiceAccountID: row.ServiceAccountID,
		Scopes:           scopesFromStrings(row.Scopes),
		Repositories:     repositories,
		ExpiresAt:        row.ExpiresAt.Time.UTC(),
		Revoked:          row.RevokedAt.Valid,
		LastUsedAt:       lastUsedAt,
		CreatedAt:        row.CreatedAt.Time.UTC(),
	}
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

func validateScopes(scopes []Scope) error {
	seen := make(map[Scope]struct{}, len(scopes))
	for _, scope := range scopes {
		switch scope {
		case ScopeArtifactRead, ScopeArtifactWrite, ScopeReleasePublish, ScopeChannelPromote, ScopeAdmin:
		default:
			return ErrInvalidRequest
		}
		if _, exists := seen[scope]; exists {
			return ErrInvalidRequest
		}
		seen[scope] = struct{}{}
	}
	return nil
}

func hasDuplicates(values []string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" {
			return true
		}
		if _, exists := seen[value]; exists {
			return true
		}
		seen[value] = struct{}{}
	}
	return false
}

func scopeStrings(scopes []Scope) []string {
	values := make([]string, len(scopes))
	for index, scope := range scopes {
		values[index] = string(scope)
	}
	return values
}

func scopesFromStrings(scopes []string) []Scope {
	values := make([]Scope, len(scopes))
	for index, scope := range scopes {
		values[index] = Scope(scope)
	}
	return values
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
