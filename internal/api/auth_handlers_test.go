package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"superfan.myasustor.com/fanchao/artifact-repository/internal/audit"
	identity "superfan.myasustor.com/fanchao/artifact-repository/internal/auth"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/idempotency"
	repositorydomain "superfan.myasustor.com/fanchao/artifact-repository/internal/repository"
)

func TestIdentityRoutesRequireAuthenticationProblemDetails(t *testing.T) {
	service := &identityServiceStub{authenticateError: identity.ErrInvalidToken}
	handler := NewServer(Dependencies{
		Readiness:     &readinessProbe{},
		Authenticator: service,
		Identity:      service,
	})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/service-accounts", nil)
	request.Header.Set("Authorization", "Bearer ar1.invalid")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
	if response.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("Content-Type = %q", response.Header().Get("Content-Type"))
	}
	var problem Problem
	if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem.Code != "invalid-token" || problem.RequestId == "" {
		t.Fatalf("problem = %+v", problem)
	}
}

func TestAuthenticationAndForbiddenFailuresAreAuditedWithoutRawRequestData(t *testing.T) {
	auditService := &auditServiceStub{}
	invalidIdentity := &identityServiceStub{authenticateError: identity.ErrInvalidToken}
	invalidHandler := NewServer(Dependencies{
		Readiness: &readinessProbe{}, Authenticator: invalidIdentity, Identity: invalidIdentity, Audit: auditService,
	})
	invalidRequest := httptest.NewRequest(http.MethodGet, "/api/v1/service-accounts", nil)
	invalidRequest.Header.Set("Authorization", "Bearer ar1.invalid-secret-value")
	invalidResponse := httptest.NewRecorder()

	invalidHandler.ServeHTTP(invalidResponse, invalidRequest)

	if invalidResponse.Code != http.StatusUnauthorized {
		t.Fatalf("invalid token status = %d, want 401", invalidResponse.Code)
	}
	if len(auditService.recorded) != 1 {
		t.Fatalf("invalid token audit events = %d, want 1", len(auditService.recorded))
	}
	assertDeniedAuditEvent(t, auditService.recorded[0], uuid.Nil, "invalid-token", "private-repo", "ar1.invalid-secret-value")

	reader := adminAPIActor()
	reader.Scopes = identity.NewScopeSet(identity.ScopeArtifactRead)
	readerIdentity := &identityServiceStub{actor: reader}
	forbiddenHandler := NewServer(Dependencies{
		Readiness:     &readinessProbe{},
		Authenticator: readerIdentity,
		Packages:      &packageServiceStub{createError: identity.ErrForbidden},
		Audit:         auditService,
	})
	forbiddenRequest := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/repositories/private-repo/packages",
		bytes.NewBufferString(`{"name":"denied-package"}`),
	)
	forbiddenRequest.Header.Set("Authorization", "Bearer ar1.reader-secret-value")
	forbiddenRequest.Header.Set("Content-Type", "application/json")
	forbiddenRequest.Header.Set("Idempotency-Key", "denied-package")
	forbiddenResponse := httptest.NewRecorder()

	forbiddenHandler.ServeHTTP(forbiddenResponse, forbiddenRequest)

	if forbiddenResponse.Code != http.StatusForbidden {
		t.Fatalf("forbidden status = %d, want 403; body = %s", forbiddenResponse.Code, forbiddenResponse.Body.String())
	}
	if len(auditService.recorded) != 2 {
		t.Fatalf("denied audit events = %d, want 2", len(auditService.recorded))
	}
	assertDeniedAuditEvent(t, auditService.recorded[1], reader.TokenID, "forbidden", "private-repo", "ar1.reader-secret-value")
}

func TestHiddenRepositoryDenialIsAuditedWithoutChangingNotFoundResponse(t *testing.T) {
	auditService := &auditServiceStub{}
	reader := adminAPIActor()
	reader.Scopes = identity.NewScopeSet(identity.ScopeArtifactRead)
	identityService := &identityServiceStub{actor: reader}
	handler := NewServer(Dependencies{
		Readiness:     &readinessProbe{},
		Authenticator: identityService,
		Repositories: &repositoryServiceStub{
			getError: errors.Join(repositorydomain.ErrNotFound, identity.ErrRepositoryDenied),
		},
		Audit: auditService,
	})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/repositories/private-repo", nil)
	request.Header.Set("Authorization", "Bearer ar1.reader-secret-value")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", response.Code, response.Body.String())
	}
	var problem Problem
	if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem.Code != "not-found" {
		t.Fatalf("problem = %+v", problem)
	}
	if len(auditService.recorded) != 1 {
		t.Fatalf("hidden denial audit events = %d, want 1", len(auditService.recorded))
	}
	assertDeniedAuditEvent(t, auditService.recorded[0], reader.TokenID, "repository-not-allowed", "private-repo", "ar1.reader-secret-value")
}

func TestHiddenRepositoryDenialFailsClosedWhenAuditIsUnavailable(t *testing.T) {
	reader := adminAPIActor()
	reader.Scopes = identity.NewScopeSet(identity.ScopeArtifactRead)
	identityService := &identityServiceStub{actor: reader}
	handler := NewServer(Dependencies{
		Readiness:     &readinessProbe{},
		Authenticator: identityService,
		Repositories: &repositoryServiceStub{
			getError: errors.Join(repositorydomain.ErrNotFound, identity.ErrRepositoryDenied),
		},
		Audit: &auditServiceStub{recordError: errors.New("database unavailable")},
	})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/repositories/private-repo", nil)
	request.Header.Set("Authorization", "Bearer ar1.reader")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", response.Code, response.Body.String())
	}
	var problem Problem
	if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem.Code != "audit-unavailable" {
		t.Fatalf("problem = %+v", problem)
	}
}

func TestCompletedDeterministicErrorWritesStoredProblemResponse(t *testing.T) {
	identityService := &identityServiceStub{actor: adminAPIActor()}
	stored := []byte(`{"type":"about:blank","title":"Conflict","status":409,"code":"conflict","requestId":"original-request"}`)
	handler := NewServer(Dependencies{
		Readiness:     &readinessProbe{},
		Authenticator: identityService,
		Repositories: &repositoryServiceStub{getError: &idempotency.CompletedError{
			Status: 409, Body: stored, Replayed: true,
		}},
	})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/repositories/repo-a", nil)
	request.Header.Set("Authorization", "Bearer ar1.valid")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("Content-Type = %q", response.Header().Get("Content-Type"))
	}
	if !bytes.Equal(response.Body.Bytes(), stored) {
		t.Fatalf("body = %s, want stored response %s", response.Body.Bytes(), stored)
	}
}

func TestAuthenticationDenialFailsClosedWhenAuditIsUnavailable(t *testing.T) {
	auditService := &auditServiceStub{recordError: errors.New("database unavailable")}
	service := &identityServiceStub{authenticateError: identity.ErrInvalidToken}
	handler := NewServer(Dependencies{
		Readiness: &readinessProbe{}, Authenticator: service, Identity: service, Audit: auditService,
	})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/service-accounts", nil)
	request.Header.Set("Authorization", "Bearer ar1.invalid")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", response.Code, response.Body.String())
	}
	var problem Problem
	if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem.Code != "audit-unavailable" || problem.RequestId == "" {
		t.Fatalf("problem = %+v", problem)
	}
}

func assertDeniedAuditEvent(t *testing.T, event audit.Event, actorID uuid.UUID, code string, forbiddenValues ...string) {
	t.Helper()
	if event.ActorTokenID != actorID || event.Action != "http.request" || event.ResourceType != "api_route" ||
		event.Outcome != audit.OutcomeDenied || event.Code != code || event.RequestID == "" || event.ResourceID == "" {
		t.Fatalf("denied audit event = %+v", event)
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("encode denied audit event: %v", err)
	}
	for _, value := range forbiddenValues {
		if strings.Contains(string(encoded), value) {
			t.Fatalf("denied audit event contains raw value %q: %s", value, encoded)
		}
	}
}

func TestCreateServiceAccountRejectsUnknownJSONFields(t *testing.T) {
	service := &identityServiceStub{actor: adminAPIActor()}
	handler := NewServer(Dependencies{
		Readiness:     &readinessProbe{},
		Authenticator: service,
		Identity:      service,
	})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/service-accounts", bytes.NewBufferString(`{"name":"edgecli-ci","extra":true}`))
	request.Header.Set("Authorization", "Bearer ar1.valid")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
	if service.createServiceAccountCalls != 0 {
		t.Fatalf("CreateServiceAccount() calls = %d", service.createServiceAccountCalls)
	}
}

func TestCreateServiceAccountPassesCanonicalIdempotencyRequest(t *testing.T) {
	createdAt := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	service := &identityServiceStub{
		actor: adminAPIActor(),
		createServiceAccountResult: identity.ServiceAccountResult{
			ID:        uuid.MustParse("cccccccc-dddd-4eee-8fff-000000000000"),
			Name:      "edgecli-ci",
			CreatedAt: createdAt,
		},
	}
	handler := NewServer(Dependencies{
		Readiness:     &readinessProbe{},
		Authenticator: service,
		Identity:      service,
	})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/service-accounts", bytes.NewBufferString(`{"name":"edgecli-ci"}`))
	request.Header.Set("Authorization", "Bearer ar1.valid")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "create-ci")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", response.Code, response.Body.String())
	}
	got := service.createServiceAccountRequest
	if got.Name != "edgecli-ci" || got.Mutation.IdempotencyKey != "create-ci" {
		t.Fatalf("CreateServiceAccount() request = %+v", got)
	}
	if got.Mutation.CanonicalResource != "/api/v1/service-accounts" || len(got.Mutation.Fingerprint) != 32 {
		t.Fatalf("mutation = %+v", got.Mutation)
	}
	if got.Mutation.Actor.TokenID != service.actor.TokenID || got.Mutation.RequestID == "" {
		t.Fatalf("mutation actor/request ID = %+v", got.Mutation)
	}

	var responseBody ServiceAccount
	if err := json.Unmarshal(response.Body.Bytes(), &responseBody); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if responseBody.Id != service.createServiceAccountResult.ID || responseBody.CreatedAt != createdAt {
		t.Fatalf("response = %+v", responseBody)
	}
}

func TestServiceAccountAndTokenReadHandlersReturnGeneratedDTOs(t *testing.T) {
	createdAt := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	serviceAccountID := uuid.MustParse("cccccccc-dddd-4eee-8fff-000000000000")
	tokenID := uuid.MustParse("dddddddd-eeee-4fff-8000-111111111111")
	service := &identityServiceStub{
		actor: adminAPIActor(),
		getServiceAccountResult: identity.ServiceAccountResult{
			ID: serviceAccountID, Name: "edgecli-ci", CreatedAt: createdAt,
		},
		listServiceAccountsResult: identity.ServiceAccountPage{
			Items: []identity.ServiceAccountResult{{ID: serviceAccountID, Name: "edgecli-ci", CreatedAt: createdAt}},
		},
		listTokensResult: identity.TokenPage{
			Items: []identity.Token{{
				ID: tokenID, ServiceAccountID: serviceAccountID,
				Scopes: []identity.Scope{identity.ScopeArtifactRead}, Repositories: []string{"releases"},
				ExpiresAt: createdAt.Add(24 * time.Hour), CreatedAt: createdAt,
			}},
		},
	}
	handler := NewServer(Dependencies{Readiness: &readinessProbe{}, Authenticator: service, Identity: service})

	requests := []struct {
		name string
		path string
	}{
		{name: "service account", path: "/api/v1/service-accounts/" + serviceAccountID.String()},
		{name: "service accounts", path: "/api/v1/service-accounts?limit=1"},
		{name: "tokens", path: "/api/v1/service-accounts/" + serviceAccountID.String() + "/tokens?limit=1"},
	}
	for _, tt := range requests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, tt.path, nil)
			request.Header.Set("Authorization", "Bearer ar1.valid")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			if tt.name == "tokens" && bytes.Contains(response.Body.Bytes(), []byte("secret")) {
				t.Fatalf("token list leaked secret: %s", response.Body.String())
			}
		})
	}
}

func TestCreateAndRevokeTokenHandlersPreserveSecretAndCanonicalScope(t *testing.T) {
	createdAt := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	serviceAccountID := uuid.MustParse("cccccccc-dddd-4eee-8fff-000000000000")
	tokenID := uuid.MustParse("dddddddd-eeee-4fff-8000-111111111111")
	service := &identityServiceStub{
		actor: adminAPIActor(),
		issueTokenResult: identity.IssuedTokenDetails{
			Token: identity.Token{
				ID: tokenID, ServiceAccountID: serviceAccountID,
				Scopes: []identity.Scope{identity.ScopeArtifactRead}, Repositories: []string{"releases"},
				ExpiresAt: createdAt.Add(24 * time.Hour), CreatedAt: createdAt,
			},
			Secret: "ar1.dddddddd-eeee-4fff-8000-111111111111.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		},
	}
	handler := NewServer(Dependencies{Readiness: &readinessProbe{}, Authenticator: service, Identity: service})

	createRequest := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/service-accounts/"+serviceAccountID.String()+"/tokens",
		bytes.NewBufferString(`{"scopes":["artifact:read"],"repositories":["releases"],"expiresAt":"2026-07-12T12:00:00Z"}`),
	)
	createRequest.Header.Set("Authorization", "Bearer ar1.valid")
	createRequest.Header.Set("Content-Type", "application/json")
	createRequest.Header.Set("Idempotency-Key", "issue-token")
	createResponse := httptest.NewRecorder()
	handler.ServeHTTP(createResponse, createRequest)
	if createResponse.Code != http.StatusCreated || !bytes.Contains(createResponse.Body.Bytes(), []byte(service.issueTokenResult.Secret)) {
		t.Fatalf("create token status = %d, body = %s", createResponse.Code, createResponse.Body.String())
	}
	if service.issueTokenRequest.Mutation.CanonicalResource != "/api/v1/service-accounts/"+serviceAccountID.String()+"/tokens" {
		t.Fatalf("issue token mutation = %+v", service.issueTokenRequest.Mutation)
	}

	revokeRequest := httptest.NewRequest(http.MethodPost, "/api/v1/tokens/"+tokenID.String()+"/revoke", nil)
	revokeRequest.Header.Set("Authorization", "Bearer ar1.valid")
	revokeRequest.Header.Set("Idempotency-Key", "revoke-token")
	revokeResponse := httptest.NewRecorder()
	handler.ServeHTTP(revokeResponse, revokeRequest)
	if revokeResponse.Code != http.StatusNoContent {
		t.Fatalf("revoke status = %d, body = %s", revokeResponse.Code, revokeResponse.Body.String())
	}
	if service.revokeTokenRequest.TokenID != tokenID || service.revokeTokenRequest.Mutation.CanonicalResource != "/api/v1/tokens/"+tokenID.String()+"/revoke" {
		t.Fatalf("revoke request = %+v", service.revokeTokenRequest)
	}
}

func TestAuditAndSigningKeyHandlersReturnPublicData(t *testing.T) {
	createdAt := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	service := &identityServiceStub{
		actor: adminAPIActor(),
		getSigningKeyResult: identity.SigningKey{
			KeyID: "release-key-01", Algorithm: "Ed25519", PublicKey: bytes.Repeat([]byte{0x31}, 32),
			Fingerprint: strings.Repeat("a", 64), Active: true, CreatedAt: createdAt,
		},
	}
	auditService := &auditServiceStub{page: audit.Page{Items: []audit.Entry{{
		ID: uuid.MustParse("eeeeeeee-ffff-4000-8111-222222222222"), Action: "token.create",
		ResourceType: "api_token", Outcome: audit.OutcomeSuccess, RequestID: "request-123",
		Details: map[string]any{"scopes": []any{"artifact:read"}}, CreatedAt: createdAt,
	}}}}
	handler := NewServer(Dependencies{
		Readiness: &readinessProbe{}, Authenticator: service, Identity: service, Audit: auditService,
	})

	for _, path := range []string{"/api/v1/audit-events", "/api/v1/signing-keys/release-key-01"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer ar1.valid")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, body = %s", path, response.Code, response.Body.String())
		}
	}
	if auditService.calls != 1 {
		t.Fatalf("audit List() calls = %d", auditService.calls)
	}
}

func adminAPIActor() identity.Actor {
	return identity.Actor{
		TokenID:          uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"),
		ServiceAccountID: uuid.MustParse("bbbbbbbb-cccc-4ddd-8eee-ffffffffffff"),
		Scopes:           identity.NewScopeSet(identity.ScopeAdmin),
		RepositoryIDs:    map[uuid.UUID]struct{}{},
	}
}

type identityServiceStub struct {
	actor                       identity.Actor
	authenticateError           error
	createServiceAccountCalls   int
	createServiceAccountRequest identity.CreateServiceAccountRequest
	createServiceAccountResult  identity.ServiceAccountResult
	getServiceAccountResult     identity.ServiceAccountResult
	listServiceAccountsResult   identity.ServiceAccountPage
	issueTokenRequest           identity.IssueTokenRequest
	issueTokenResult            identity.IssuedTokenDetails
	listTokensResult            identity.TokenPage
	revokeTokenRequest          identity.RevokeTokenRequest
	getSigningKeyResult         identity.SigningKey
}

func (s *identityServiceStub) Authenticate(context.Context, string) (identity.Actor, error) {
	return s.actor, s.authenticateError
}

func (s *identityServiceStub) CreateServiceAccount(_ context.Context, request identity.CreateServiceAccountRequest) (identity.ServiceAccountResult, error) {
	s.createServiceAccountCalls++
	s.createServiceAccountRequest = request
	return s.createServiceAccountResult, nil
}

func (s *identityServiceStub) GetServiceAccount(context.Context, identity.Actor, uuid.UUID) (identity.ServiceAccountResult, error) {
	return s.getServiceAccountResult, nil
}

func (s *identityServiceStub) ListServiceAccounts(context.Context, identity.Actor, identity.ListRequest) (identity.ServiceAccountPage, error) {
	return s.listServiceAccountsResult, nil
}

func (s *identityServiceStub) IssueToken(_ context.Context, request identity.IssueTokenRequest) (identity.IssuedTokenDetails, error) {
	s.issueTokenRequest = request
	return s.issueTokenResult, nil
}

func (s *identityServiceStub) ListTokens(context.Context, identity.Actor, uuid.UUID, identity.ListRequest) (identity.TokenPage, error) {
	return s.listTokensResult, nil
}

func (s *identityServiceStub) RevokeToken(_ context.Context, request identity.RevokeTokenRequest) (identity.RevokeTokenResult, error) {
	s.revokeTokenRequest = request
	return identity.RevokeTokenResult{}, nil
}

func (s *identityServiceStub) GetSigningKey(context.Context, identity.Actor, string) (identity.SigningKey, error) {
	return s.getSigningKeyResult, nil
}

type auditServiceStub struct {
	page        audit.Page
	calls       int
	recorded    []audit.Event
	recordError error
}

func (s *auditServiceStub) List(context.Context, audit.ListRequest) (audit.Page, error) {
	s.calls++
	return s.page, nil
}

func (s *auditServiceStub) RecordStandalone(_ context.Context, event audit.Event) (audit.Entry, error) {
	if s.recordError != nil {
		return audit.Entry{}, s.recordError
	}
	s.recorded = append(s.recorded, event)
	return audit.Entry{}, nil
}
