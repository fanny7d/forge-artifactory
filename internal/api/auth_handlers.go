package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	artifactdomain "superfan.myasustor.com/fanchao/artifact-repository/internal/artifact"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/audit"
	identity "superfan.myasustor.com/fanchao/artifact-repository/internal/auth"
	blobdomain "superfan.myasustor.com/fanchao/artifact-repository/internal/blob"
	channeldomain "superfan.myasustor.com/fanchao/artifact-repository/internal/channel"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/idempotency"
	releasedomain "superfan.myasustor.com/fanchao/artifact-repository/internal/release"
	repositorydomain "superfan.myasustor.com/fanchao/artifact-repository/internal/repository"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/storage"
)

const maxJSONBodyBytes int64 = 64 << 10

var validIdempotencyKey = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

type IdentityService interface {
	CreateServiceAccount(context.Context, identity.CreateServiceAccountRequest) (identity.ServiceAccountResult, error)
	GetServiceAccount(context.Context, identity.Actor, uuid.UUID) (identity.ServiceAccountResult, error)
	ListServiceAccounts(context.Context, identity.Actor, identity.ListRequest) (identity.ServiceAccountPage, error)
	IssueToken(context.Context, identity.IssueTokenRequest) (identity.IssuedTokenDetails, error)
	ListTokens(context.Context, identity.Actor, uuid.UUID, identity.ListRequest) (identity.TokenPage, error)
	RevokeToken(context.Context, identity.RevokeTokenRequest) (identity.RevokeTokenResult, error)
	GetSigningKey(context.Context, identity.Actor, string) (identity.SigningKey, error)
}

type AuditService interface {
	List(context.Context, audit.ListRequest) (audit.Page, error)
	RecordStandalone(context.Context, audit.Event) (audit.Entry, error)
}

type authHandlers struct {
	identity IdentityService
	audit    AuditService
}

func registerAuthRoutes(router chi.Router, identityService IdentityService, auditService AuditService) {
	handlers := authHandlers{identity: identityService, audit: auditService}
	if identityService != nil {
		router.Route("/service-accounts", func(router chi.Router) {
			router.Post("/", handlers.createServiceAccount)
			router.Get("/", handlers.listServiceAccounts)
			router.Get("/{id}", handlers.getServiceAccount)
			router.Post("/{id}/tokens", handlers.createToken)
			router.Get("/{id}/tokens", handlers.listTokens)
		})
		router.Post("/tokens/{id}/revoke", handlers.revokeToken)
		router.Get("/signing-keys/{keyId}", handlers.getSigningKey)
	}
	if auditService != nil {
		router.Get("/audit-events", handlers.listAuditEvents)
	}
}

func (h authHandlers) createServiceAccount(w http.ResponseWriter, r *http.Request) {
	var body CreateServiceAccountRequest
	raw, err := decodeJSONRequest(w, r, &body)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	actor, ok := identity.ActorFromContext(r.Context())
	if !ok {
		writeAuthenticationError(w, r, identity.ErrInvalidToken)
		return
	}
	mutation, err := mutationFromRequest(r, actor, "/api/v1/service-accounts", raw)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	created, err := h.identity.CreateServiceAccount(r.Context(), identity.CreateServiceAccountRequest{
		Mutation: mutation,
		Name:     body.Name,
	})
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, serviceAccountDTO(created))
}

func (h authHandlers) listServiceAccounts(w http.ResponseWriter, r *http.Request) {
	actor, ok := identity.ActorFromContext(r.Context())
	if !ok {
		writeAuthenticationError(w, r, identity.ErrInvalidToken)
		return
	}
	request, err := identityListRequest(r, "service-accounts")
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	page, err := h.identity.ListServiceAccounts(r.Context(), actor, request)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	items := make([]ServiceAccount, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, serviceAccountDTO(item))
	}
	writeJSON(w, http.StatusOK, ServiceAccountPage{
		Items:      items,
		NextCursor: encodeIdentityCursor("service-accounts", page.Next),
	})
}

func (h authHandlers) getServiceAccount(w http.ResponseWriter, r *http.Request) {
	actor, ok := identity.ActorFromContext(r.Context())
	if !ok {
		writeAuthenticationError(w, r, identity.ErrInvalidToken)
		return
	}
	serviceAccountID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeHandlerError(w, r, identity.ErrInvalidRequest)
		return
	}
	account, err := h.identity.GetServiceAccount(r.Context(), actor, serviceAccountID)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, serviceAccountDTO(account))
}

func (h authHandlers) createToken(w http.ResponseWriter, r *http.Request) {
	actor, ok := identity.ActorFromContext(r.Context())
	if !ok {
		writeAuthenticationError(w, r, identity.ErrInvalidToken)
		return
	}
	serviceAccountID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeHandlerError(w, r, identity.ErrInvalidRequest)
		return
	}
	var body CreateTokenRequest
	raw, err := decodeJSONRequest(w, r, &body)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	canonicalResource := "/api/v1/service-accounts/" + serviceAccountID.String() + "/tokens"
	mutation, err := mutationFromRequest(r, actor, canonicalResource, raw)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	scopes := make([]identity.Scope, len(body.Scopes))
	for index, scope := range body.Scopes {
		scopes[index] = identity.Scope(scope)
	}
	repositories := make([]string, len(body.Repositories))
	copy(repositories, body.Repositories)
	issued, err := h.identity.IssueToken(r.Context(), identity.IssueTokenRequest{
		Mutation:         mutation,
		ServiceAccountID: serviceAccountID,
		Scopes:           scopes,
		Repositories:     repositories,
		ExpiresAt:        body.ExpiresAt,
	})
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, issuedTokenDTO(issued))
}

func (h authHandlers) listTokens(w http.ResponseWriter, r *http.Request) {
	actor, ok := identity.ActorFromContext(r.Context())
	if !ok {
		writeAuthenticationError(w, r, identity.ErrInvalidToken)
		return
	}
	serviceAccountID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeHandlerError(w, r, identity.ErrInvalidRequest)
		return
	}
	request, err := identityListRequest(r, "tokens:"+serviceAccountID.String())
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	page, err := h.identity.ListTokens(r.Context(), actor, serviceAccountID, request)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	items := make([]Token, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, tokenDTO(item))
	}
	writeJSON(w, http.StatusOK, TokenPage{
		Items:      items,
		NextCursor: encodeIdentityCursor("tokens:"+serviceAccountID.String(), page.Next),
	})
}

func (h authHandlers) revokeToken(w http.ResponseWriter, r *http.Request) {
	actor, ok := identity.ActorFromContext(r.Context())
	if !ok {
		writeAuthenticationError(w, r, identity.ErrInvalidToken)
		return
	}
	tokenID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeHandlerError(w, r, identity.ErrInvalidRequest)
		return
	}
	canonicalResource := "/api/v1/tokens/" + tokenID.String() + "/revoke"
	mutation, err := mutationFromRequest(r, actor, canonicalResource, nil)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	if _, err := h.identity.RevokeToken(r.Context(), identity.RevokeTokenRequest{
		Mutation: mutation,
		TokenID:  tokenID,
	}); err != nil {
		writeHandlerError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h authHandlers) listAuditEvents(w http.ResponseWriter, r *http.Request) {
	actor, ok := identity.ActorFromContext(r.Context())
	if !ok {
		writeAuthenticationError(w, r, identity.ErrInvalidToken)
		return
	}
	if err := identity.Require(actor, identity.ScopeAdmin, uuid.Nil); err != nil {
		writeHandlerError(w, r, err)
		return
	}
	request, err := auditListRequest(r)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	page, err := h.audit.List(r.Context(), request)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	items := make([]AuditEvent, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, auditEventDTO(item))
	}
	writeJSON(w, http.StatusOK, AuditEventPage{
		Items:      items,
		NextCursor: encodeAuditCursor(page.Next),
	})
}

func (h authHandlers) getSigningKey(w http.ResponseWriter, r *http.Request) {
	actor, ok := identity.ActorFromContext(r.Context())
	if !ok {
		writeAuthenticationError(w, r, identity.ErrInvalidToken)
		return
	}
	key, err := h.identity.GetSigningKey(r.Context(), actor, chi.URLParam(r, "keyId"))
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, SigningKey{
		KeyId:       key.KeyID,
		Algorithm:   key.Algorithm,
		PublicKey:   base64.StdEncoding.EncodeToString(key.PublicKey),
		Fingerprint: key.Fingerprint,
		Active:      key.Active,
		CreatedAt:   key.CreatedAt,
	})
}

func mutationFromRequest(r *http.Request, actor identity.Actor, canonicalResource string, body []byte) (identity.Mutation, error) {
	key := r.Header.Get("Idempotency-Key")
	if key != "" && !validIdempotencyKey.MatchString(key) {
		return identity.Mutation{}, identity.ErrInvalidRequest
	}
	return identity.Mutation{
		Actor:             actor,
		Method:            r.Method,
		RequestID:         requestIDFromContext(r.Context()),
		IdempotencyKey:    key,
		Fingerprint:       requestFingerprint(r.Method, canonicalResource, normalizedContentType(r.Header.Get("Content-Type")), body),
		CanonicalResource: canonicalResource,
	}, nil
}

func requestFingerprint(method, canonicalResource, contentType string, body []byte) []byte {
	bodyHash := sha256.Sum256(body)
	hash := sha256.New()
	writeFingerprintField(hash, method)
	writeFingerprintField(hash, canonicalResource)
	writeFingerprintField(hash, contentType)
	_, _ = hash.Write(bodyHash[:])
	return hash.Sum(nil)
}

func writeFingerprintField(writer io.Writer, value string) {
	_ = binary.Write(writer, binary.BigEndian, uint32(len(value)))
	_, _ = io.WriteString(writer, value)
}

func normalizedContentType(value string) string {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(value))
	}
	return strings.ToLower(mediaType)
}

func decodeJSONRequest(w http.ResponseWriter, r *http.Request, destination any) ([]byte, error) {
	if normalizedContentType(r.Header.Get("Content-Type")) != "application/json" {
		return nil, fmt.Errorf("%w: Content-Type must be application/json", identity.ErrInvalidRequest)
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: read JSON body", identity.ErrInvalidRequest)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return nil, fmt.Errorf("%w: decode JSON body", identity.ErrInvalidRequest)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: JSON body must contain one value", identity.ErrInvalidRequest)
	}
	return raw, nil
}

type cursorEnvelope struct {
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"createdAt"`
	ID        uuid.UUID `json:"id"`
	Key       string    `json:"key,omitempty"`
}

func identityListRequest(r *http.Request, kind string) (identity.ListRequest, error) {
	limit, err := listLimit(r)
	if err != nil {
		return identity.ListRequest{}, err
	}
	request := identity.ListRequest{Limit: limit}
	encoded := r.URL.Query().Get("cursor")
	if encoded == "" {
		return request, nil
	}
	cursor, err := decodeCursor(encoded, kind)
	if err != nil {
		return identity.ListRequest{}, err
	}
	request.After = &identity.Cursor{CreatedAt: cursor.CreatedAt, ID: cursor.ID}
	return request, nil
}

func auditListRequest(r *http.Request) (audit.ListRequest, error) {
	limit, err := listLimit(r)
	if err != nil {
		return audit.ListRequest{}, err
	}
	request := audit.ListRequest{Limit: limit}
	encoded := r.URL.Query().Get("cursor")
	if encoded == "" {
		return request, nil
	}
	cursor, err := decodeCursor(encoded, "audit-events")
	if err != nil {
		return audit.ListRequest{}, err
	}
	request.After = &audit.Cursor{CreatedAt: cursor.CreatedAt, ID: cursor.ID}
	return request, nil
}

func listLimit(r *http.Request) (int32, error) {
	value := r.URL.Query().Get("limit")
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 32)
	if err != nil || parsed < 1 || parsed > 200 {
		return 0, identity.ErrInvalidRequest
	}
	return int32(parsed), nil
}

func decodeCursor(encoded, kind string) (cursorEnvelope, error) {
	if len(encoded) > 512 {
		return cursorEnvelope{}, identity.ErrInvalidRequest
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return cursorEnvelope{}, identity.ErrInvalidRequest
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var cursor cursorEnvelope
	if err := decoder.Decode(&cursor); err != nil || cursor.Kind != kind || cursor.ID == uuid.Nil || cursor.CreatedAt.IsZero() {
		return cursorEnvelope{}, identity.ErrInvalidRequest
	}
	return cursor, nil
}

func encodeIdentityCursor(kind string, cursor *identity.Cursor) *PageCursor {
	if cursor == nil {
		return nil
	}
	return encodeCursor(cursorEnvelope{Kind: kind, CreatedAt: cursor.CreatedAt, ID: cursor.ID})
}

func encodeAuditCursor(cursor *audit.Cursor) *PageCursor {
	if cursor == nil {
		return nil
	}
	return encodeCursor(cursorEnvelope{Kind: "audit-events", CreatedAt: cursor.CreatedAt, ID: cursor.ID})
}

func encodeCursor(cursor cursorEnvelope) *PageCursor {
	raw, err := json.Marshal(cursor)
	if err != nil {
		return nil
	}
	encoded := PageCursor(base64.RawURLEncoding.EncodeToString(raw))
	return &encoded
}

func serviceAccountDTO(account identity.ServiceAccountResult) ServiceAccount {
	return ServiceAccount{Id: account.ID, Name: account.Name, CreatedAt: account.CreatedAt}
}

func tokenDTO(token identity.Token) Token {
	scopes := make([]Scope, len(token.Scopes))
	for index, scope := range token.Scopes {
		scopes[index] = Scope(scope)
	}
	repositories := make([]Name, len(token.Repositories))
	copy(repositories, token.Repositories)
	return Token{
		Id:               token.ID,
		ServiceAccountId: token.ServiceAccountID,
		Scopes:           scopes,
		Repositories:     repositories,
		ExpiresAt:        token.ExpiresAt,
		Revoked:          token.Revoked,
		LastUsedAt:       token.LastUsedAt,
		CreatedAt:        token.CreatedAt,
	}
}

func issuedTokenDTO(token identity.IssuedTokenDetails) IssuedToken {
	details := tokenDTO(token.Token)
	return IssuedToken{
		Id:               details.Id,
		ServiceAccountId: details.ServiceAccountId,
		Scopes:           details.Scopes,
		Repositories:     details.Repositories,
		ExpiresAt:        details.ExpiresAt,
		Revoked:          details.Revoked,
		LastUsedAt:       details.LastUsedAt,
		CreatedAt:        details.CreatedAt,
		Secret:           token.Secret,
	}
}

func auditEventDTO(event audit.Entry) AuditEvent {
	var details *map[string]any
	if len(event.Details) > 0 {
		value := event.Details
		details = &value
	}
	return AuditEvent{
		Id:           event.ID,
		ActorId:      event.ActorTokenID,
		RepositoryId: event.RepositoryID,
		Action:       event.Action,
		ResourceType: event.ResourceType,
		ResourceId:   event.ResourceID,
		Outcome:      AuditEventOutcome(event.Outcome),
		Code:         event.Code,
		RequestId:    event.RequestID,
		Details:      details,
		CreatedAt:    event.CreatedAt,
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeAuthenticationError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, identity.ErrInvalidToken) {
		if auditErr := recordDeniedRequest(r, "invalid-token"); auditErr != nil {
			writeAuditUnavailable(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeRequestProblem(w, r, Problem{
			Type:   "about:blank",
			Title:  "Unauthorized",
			Status: http.StatusUnauthorized,
			Code:   "invalid-token",
		})
		return
	}
	writeRequestProblem(w, r, Problem{
		Type:   "about:blank",
		Title:  "Service Unavailable",
		Status: http.StatusServiceUnavailable,
		Code:   "authentication-unavailable",
	})
}

func writeHandlerError(w http.ResponseWriter, r *http.Request, err error) {
	if completed, ok := idempotency.CompletedErrorFrom(err); ok {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(completed.Status)
		_, _ = w.Write(completed.Body)
		return
	}
	problem := Problem{Type: "about:blank", Title: "Internal Server Error", Status: http.StatusInternalServerError, Code: "internal-error"}
	switch {
	case errors.Is(err, identity.ErrInvalidToken):
		writeAuthenticationError(w, r, err)
		return
	case errors.Is(err, identity.ErrForbidden):
		if auditErr := recordDeniedRequest(r, "forbidden"); auditErr != nil {
			writeAuditUnavailable(w, r)
			return
		}
		problem.Title = "Forbidden"
		problem.Status = http.StatusForbidden
		problem.Code = "forbidden"
	case errors.Is(err, identity.ErrRepositoryDenied):
		if auditErr := recordDeniedRequest(r, "repository-not-allowed"); auditErr != nil {
			writeAuditUnavailable(w, r)
			return
		}
		problem.Title = "Not Found"
		problem.Status = http.StatusNotFound
		problem.Code = "not-found"
	case errors.Is(err, identity.ErrNotFound), errors.Is(err, repositorydomain.ErrNotFound), errors.Is(err, releasedomain.ErrNotFound), errors.Is(err, channeldomain.ErrNotFound), errors.Is(err, artifactdomain.ErrNotFound), errors.Is(err, blobdomain.ErrNotFound), errors.Is(err, storage.ErrNotFound):
		problem.Title = "Not Found"
		problem.Status = http.StatusNotFound
		problem.Code = "not-found"
	case errors.Is(err, identity.ErrConflict), errors.Is(err, repositorydomain.ErrConflict), errors.Is(err, releasedomain.ErrConflict), errors.Is(err, releasedomain.ErrPublishNotReady), errors.Is(err, releasedomain.ErrLeaseLost), errors.Is(err, channeldomain.ErrConflict), errors.Is(err, channeldomain.ErrReleaseNotPublished), errors.Is(err, artifactdomain.ErrConflict), errors.Is(err, artifactdomain.ErrUploadLeaseLost), errors.Is(err, blobdomain.ErrInProgress), errors.Is(err, blobdomain.ErrDeleting), errors.Is(err, blobdomain.ErrNotReady), errors.Is(err, blobdomain.ErrSizeMismatch), errors.Is(err, storage.ErrObjectConflict):
		problem.Title = "Conflict"
		problem.Status = http.StatusConflict
		problem.Code = "conflict"
	case errors.Is(err, identity.ErrInvalidRequest), errors.Is(err, repositorydomain.ErrInvalidRequest), errors.Is(err, releasedomain.ErrInvalidRequest), errors.Is(err, channeldomain.ErrInvalidRequest), errors.Is(err, artifactdomain.ErrInvalidRequest), errors.Is(err, artifactdomain.ErrInvalidPath), errors.Is(err, artifactdomain.ErrLengthMismatch), errors.Is(err, artifactdomain.ErrUploadIdle), errors.Is(err, storage.ErrInvalidRange), errors.Is(err, audit.ErrInvalidPage):
		problem.Title = "Bad Request"
		problem.Status = http.StatusBadRequest
		problem.Code = "invalid-request"
	case errors.Is(err, releasedomain.ErrUnprocessable):
		problem.Title = "Unprocessable Entity"
		problem.Status = http.StatusUnprocessableEntity
		problem.Code = "cross-repository-artifact"
	case errors.Is(err, releasedomain.ErrNoArtifacts):
		problem.Title = "Unprocessable Entity"
		problem.Status = http.StatusUnprocessableEntity
		problem.Code = "release-has-no-artifacts"
	case errors.Is(err, channeldomain.ErrReleaseNotInPackage):
		problem.Title = "Unprocessable Entity"
		problem.Status = http.StatusUnprocessableEntity
		problem.Code = "release-not-in-package"
	case errors.Is(err, artifactdomain.ErrChecksumMismatch):
		problem.Title = "Unprocessable Entity"
		problem.Status = http.StatusUnprocessableEntity
		problem.Code = "checksum-mismatch"
	case errors.Is(err, artifactdomain.ErrTooLarge):
		problem.Title = "Payload Too Large"
		problem.Status = http.StatusRequestEntityTooLarge
		problem.Code = "payload-too-large"
	case errors.Is(err, artifactdomain.ErrPublicEndpointUnavailable):
		problem.Title = "Conflict"
		problem.Status = http.StatusConflict
		problem.Code = "public-endpoint-unavailable"
	case errors.Is(err, idempotency.ErrKeyConflict):
		problem.Title = "Conflict"
		problem.Status = http.StatusConflict
		problem.Code = "idempotency-key-conflict"
	case errors.Is(err, idempotency.ErrInProgress):
		problem.Title = "Conflict"
		problem.Status = http.StatusConflict
		problem.Code = "idempotency-in-progress"
		w.Header().Set("Retry-After", "1")
	case errors.Is(err, releasedomain.ErrPublishPending):
		problem.Title = "Service Unavailable"
		problem.Status = http.StatusServiceUnavailable
		problem.Code = "publish-pending"
		w.Header().Set("Retry-After", "5")
	case errors.Is(err, releasedomain.ErrPublishAborted):
		problem.Title = "Service Unavailable"
		problem.Status = http.StatusServiceUnavailable
		problem.Code = "publish-attempt-aborted"
	case errors.Is(err, releasedomain.ErrPublishFailed):
		problem.Title = "Internal Server Error"
		problem.Status = http.StatusInternalServerError
		problem.Code = "publish-failed"
	}
	writeRequestProblem(w, r, problem)
}
