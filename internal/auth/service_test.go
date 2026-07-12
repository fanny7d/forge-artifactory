package auth

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"superfan.myasustor.com/fanchao/artifact-repository/internal/audit"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/clock"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/database"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/idempotency"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/testenv"
)

func TestCreateServiceAccountCommitsAuditAndReplays(t *testing.T) {
	pool, service, admin, _ := newAuthTestService(t)
	request := CreateServiceAccountRequest{
		Mutation: fixedMutation(admin, "/api/v1/service-accounts", "create-ci"),
		Name:     "edgecli-ci",
	}

	first, err := service.CreateServiceAccount(t.Context(), request)
	if err != nil {
		t.Fatalf("first CreateServiceAccount() error = %v", err)
	}
	second, err := service.CreateServiceAccount(t.Context(), request)
	if err != nil {
		t.Fatalf("replay CreateServiceAccount() error = %v", err)
	}
	if first.ID != second.ID || first.Name != "edgecli-ci" {
		t.Fatalf("first = %+v, replay = %+v", first, second)
	}
	if first.Replayed || !second.Replayed {
		t.Fatalf("first replayed = %v, second replayed = %v", first.Replayed, second.Replayed)
	}

	var accounts, events int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM service_accounts WHERE name = 'edgecli-ci'").Scan(&accounts); err != nil {
		t.Fatalf("count service accounts: %v", err)
	}
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM audit_events WHERE action = 'service-account.create'").Scan(&events); err != nil {
		t.Fatalf("count audit events: %v", err)
	}
	if accounts != 1 || events != 1 {
		t.Fatalf("accounts = %d, events = %d, want 1 and 1", accounts, events)
	}
}

func TestCreateServiceAccountConflictCompletesAndReplays(t *testing.T) {
	pool, service, admin, _ := newAuthTestService(t)
	if _, err := service.CreateServiceAccount(t.Context(), CreateServiceAccountRequest{
		Mutation: fixedMutation(admin, "/api/v1/service-accounts", "create-existing-account"), Name: "existing-account",
	}); err != nil {
		t.Fatalf("seed CreateServiceAccount() error = %v", err)
	}
	request := CreateServiceAccountRequest{
		Mutation: fixedMutation(admin, "/api/v1/service-accounts", "create-account-conflict"), Name: "existing-account",
	}

	_, err := service.CreateServiceAccount(t.Context(), request)
	completed, ok := idempotency.CompletedErrorFrom(err)
	if !ok || completed.Replayed || completed.Status != 409 || !errors.Is(err, ErrConflict) {
		t.Fatalf("CreateServiceAccount() conflict = %+v, err = %v", completed, err)
	}
	_, err = service.CreateServiceAccount(t.Context(), request)
	replayed, ok := idempotency.CompletedErrorFrom(err)
	if !ok || !replayed.Replayed || replayed.Status != 409 {
		t.Fatalf("CreateServiceAccount() conflict replay = %+v, err = %v", replayed, err)
	}

	var records, failedAudits int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM idempotency_records WHERE idempotency_key = 'create-account-conflict' AND state = 'completed' AND http_status = 409").Scan(&records); err != nil {
		t.Fatalf("count conflict records: %v", err)
	}
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM audit_events WHERE request_id = 'request-123' AND action = 'service-account.create' AND outcome = 'failed' AND code = 'conflict'").Scan(&failedAudits); err != nil {
		t.Fatalf("count conflict audits: %v", err)
	}
	if records != 1 || failedAudits != 1 {
		t.Fatalf("conflict records = %d, failed audits = %d, want 1 and 1", records, failedAudits)
	}
}

func TestIssueTokenStoresOnlyHMACAndReplaysEncryptedSecret(t *testing.T) {
	pool, service, admin, _ := newAuthTestService(t)
	serviceAccountID := uuid.MustParse("22222222-3333-4444-8555-666666666666")
	repositoryID := uuid.MustParse("33333333-4444-4555-8666-777777777777")
	if _, err := pool.Exec(t.Context(), "INSERT INTO service_accounts (id, name) VALUES ($1, 'release-ci')", serviceAccountID); err != nil {
		t.Fatalf("insert service account: %v", err)
	}
	if _, err := pool.Exec(t.Context(), "INSERT INTO repositories (id, key, display_name, created_by) VALUES ($1, 'releases', 'Releases', $2)", repositoryID, admin.TokenID); err != nil {
		t.Fatalf("insert repository: %v", err)
	}

	request := IssueTokenRequest{
		Mutation:         fixedMutation(admin, "/api/v1/service-accounts/"+serviceAccountID.String()+"/tokens", "issue-token"),
		ServiceAccountID: serviceAccountID,
		Scopes:           []Scope{ScopeArtifactRead},
		Repositories:     []string{"releases"},
		ExpiresAt:        fixedAuthTime.Add(24 * time.Hour),
	}
	first, err := service.IssueToken(t.Context(), request)
	if err != nil {
		t.Fatalf("first IssueToken() error = %v", err)
	}
	second, err := service.IssueToken(t.Context(), request)
	if err != nil {
		t.Fatalf("replay IssueToken() error = %v", err)
	}
	if first.Secret == "" || second.Secret != first.Secret || second.ID != first.ID {
		t.Fatalf("first = %+v, replay = %+v", first, second)
	}
	if first.Replayed || !second.Replayed {
		t.Fatalf("first replayed = %v, second replayed = %v", first.Replayed, second.Replayed)
	}

	var storedHMAC, storedResponse []byte
	var encrypted bool
	if err := pool.QueryRow(t.Context(), "SELECT secret_hmac FROM api_tokens WHERE id = $1", first.ID).Scan(&storedHMAC); err != nil {
		t.Fatalf("read stored HMAC: %v", err)
	}
	if err := pool.QueryRow(t.Context(), "SELECT response_body, response_encrypted FROM idempotency_records WHERE idempotency_key = 'issue-token'").Scan(&storedResponse, &encrypted); err != nil {
		t.Fatalf("read idempotency response: %v", err)
	}
	if len(storedHMAC) != 32 || bytes.Contains(storedHMAC, []byte(first.Secret)) {
		t.Fatalf("stored HMAC = %x", storedHMAC)
	}
	if !encrypted || bytes.Contains(storedResponse, []byte(first.Secret)) {
		t.Fatalf("encrypted = %v, stored response contains plaintext = %v", encrypted, bytes.Contains(storedResponse, []byte(first.Secret)))
	}

	actor, err := service.Authenticate(t.Context(), first.Secret)
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if actor.TokenID != first.ID || actor.ServiceAccountID != serviceAccountID {
		t.Fatalf("authenticated actor = %+v", actor)
	}
	if err := Require(actor, ScopeArtifactRead, repositoryID); err != nil {
		t.Fatalf("Require() error = %v", err)
	}
}

func TestRevokeTokenMakesAuthenticationFailAndReplaysAudit(t *testing.T) {
	pool, service, admin, _ := newAuthTestService(t)
	tokenID := uuid.MustParse("44444444-5555-4666-8777-888888888888")
	serviceAccountID := uuid.MustParse("55555555-6666-4777-8888-999999999999")
	if _, err := pool.Exec(t.Context(), "INSERT INTO service_accounts (id, name) VALUES ($1, 'revoked-ci')", serviceAccountID); err != nil {
		t.Fatalf("insert service account: %v", err)
	}
	issued, err := IssueToken(bytes.NewReader(bytes.Repeat([]byte{0x71}, 32)), tokenID, testPepper)
	if err != nil {
		t.Fatalf("IssueToken() error = %v", err)
	}
	if _, err := pool.Exec(
		t.Context(),
		`INSERT INTO api_tokens
		 (id, service_account_id, secret_hmac, scopes, repository_ids, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		tokenID,
		serviceAccountID,
		issued.SecretHMAC,
		[]string{"artifact:read"},
		[]uuid.UUID{},
		fixedAuthTime.Add(24*time.Hour),
	); err != nil {
		t.Fatalf("insert API token: %v", err)
	}
	if _, err := service.Authenticate(t.Context(), issued.Bearer); err != nil {
		t.Fatalf("Authenticate() before revoke error = %v", err)
	}

	request := RevokeTokenRequest{
		Mutation: fixedMutation(admin, "/api/v1/tokens/"+tokenID.String()+"/revoke", "revoke-token"),
		TokenID:  tokenID,
	}
	first, err := service.RevokeToken(t.Context(), request)
	if err != nil {
		t.Fatalf("first RevokeToken() error = %v", err)
	}
	second, err := service.RevokeToken(t.Context(), request)
	if err != nil {
		t.Fatalf("replay RevokeToken() error = %v", err)
	}
	if first.Replayed || !second.Replayed {
		t.Fatalf("first replayed = %v, second replayed = %v", first.Replayed, second.Replayed)
	}
	if _, err := service.Authenticate(t.Context(), issued.Bearer); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("Authenticate() after revoke error = %v, want ErrInvalidToken", err)
	}

	var events int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM audit_events WHERE action = 'token.revoke'").Scan(&events); err != nil {
		t.Fatalf("count audit events: %v", err)
	}
	if events != 1 {
		t.Fatalf("audit events = %d, want 1", events)
	}
}

func TestServiceAccountAndTokenReadsUseStableCursor(t *testing.T) {
	pool, service, admin, _ := newAuthTestService(t)
	olderID := uuid.MustParse("66666666-7777-4888-8999-aaaaaaaaaaaa")
	newerID := uuid.MustParse("77777777-8888-4999-8aaa-bbbbbbbbbbbb")
	if _, err := pool.Exec(t.Context(), "UPDATE service_accounts SET created_at = $1 WHERE id = $2", fixedAuthTime.Add(-3*time.Hour), admin.ServiceAccountID); err != nil {
		t.Fatalf("move admin creation time: %v", err)
	}
	if _, err := pool.Exec(
		t.Context(),
		"INSERT INTO service_accounts (id, name, created_at) VALUES ($1, 'older-ci', $2), ($3, 'newer-ci', $4)",
		olderID, fixedAuthTime.Add(-2*time.Hour), newerID, fixedAuthTime.Add(-time.Hour),
	); err != nil {
		t.Fatalf("insert service accounts: %v", err)
	}

	first, err := service.ListServiceAccounts(t.Context(), admin, ListRequest{Limit: 1})
	if err != nil {
		t.Fatalf("ListServiceAccounts() first page error = %v", err)
	}
	second, err := service.ListServiceAccounts(t.Context(), admin, ListRequest{Limit: 1, After: first.Next})
	if err != nil {
		t.Fatalf("ListServiceAccounts() second page error = %v", err)
	}
	if len(first.Items) != 1 || first.Items[0].ID != newerID || first.Next == nil {
		t.Fatalf("first service-account page = %+v", first)
	}
	if len(second.Items) != 1 || second.Items[0].ID != olderID || second.Next == nil {
		t.Fatalf("second service-account page = %+v", second)
	}
	account, err := service.GetServiceAccount(t.Context(), admin, newerID)
	if err != nil || account.Name != "newer-ci" {
		t.Fatalf("GetServiceAccount() = %+v, %v", account, err)
	}

	repositoryID := uuid.MustParse("88888888-9999-4aaa-8bbb-cccccccccccc")
	if _, err := pool.Exec(t.Context(), "INSERT INTO repositories (id, key, display_name, created_by) VALUES ($1, 'releases', 'Releases', $2)", repositoryID, admin.TokenID); err != nil {
		t.Fatalf("insert repository: %v", err)
	}
	for index, tokenID := range []uuid.UUID{
		uuid.MustParse("99999999-aaaa-4bbb-8ccc-dddddddddddd"),
		uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-111111111111"),
	} {
		if _, err := pool.Exec(
			t.Context(),
			`INSERT INTO api_tokens
			 (id, service_account_id, secret_hmac, scopes, repository_ids, expires_at, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			tokenID,
			newerID,
			bytes.Repeat([]byte{byte(index + 1)}, 32),
			[]string{"artifact:read"},
			[]uuid.UUID{repositoryID},
			fixedAuthTime.Add(24*time.Hour),
			fixedAuthTime.Add(time.Duration(index)*time.Hour),
		); err != nil {
			t.Fatalf("insert token %d: %v", index, err)
		}
	}
	firstTokens, err := service.ListTokens(t.Context(), admin, newerID, ListRequest{Limit: 1})
	if err != nil {
		t.Fatalf("ListTokens() first page error = %v", err)
	}
	secondTokens, err := service.ListTokens(t.Context(), admin, newerID, ListRequest{Limit: 1, After: firstTokens.Next})
	if err != nil {
		t.Fatalf("ListTokens() second page error = %v", err)
	}
	if len(firstTokens.Items) != 1 || len(secondTokens.Items) != 1 || firstTokens.Items[0].ID == secondTokens.Items[0].ID {
		t.Fatalf("token pages = %+v, %+v", firstTokens, secondTokens)
	}
	if len(firstTokens.Items[0].Repositories) != 1 || firstTokens.Items[0].Repositories[0] != "releases" {
		t.Fatalf("token repositories = %#v", firstTokens.Items[0].Repositories)
	}
}

var (
	fixedAuthTime = time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	testPepper    = bytes.Repeat([]byte{0x41}, 32)
)

func newAuthTestService(t *testing.T) (*pgxpool.Pool, *Service, Actor, string) {
	t.Helper()
	pool := testenv.StartPostgres(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}

	serviceAccountID := uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	tokenID := uuid.MustParse("bbbbbbbb-cccc-4ddd-8eee-ffffffffffff")
	issued, err := IssueToken(bytes.NewReader(bytes.Repeat([]byte{0x61}, 32)), tokenID, testPepper)
	if err != nil {
		t.Fatalf("IssueToken() error = %v", err)
	}
	if _, err := pool.Exec(t.Context(), "INSERT INTO service_accounts (id, name) VALUES ($1, 'test-admin')", serviceAccountID); err != nil {
		t.Fatalf("insert admin service account: %v", err)
	}
	if _, err := pool.Exec(
		t.Context(),
		`INSERT INTO api_tokens
		 (id, service_account_id, secret_hmac, scopes, repository_ids, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		tokenID,
		serviceAccountID,
		issued.SecretHMAC,
		[]string{"admin"},
		[]uuid.UUID{},
		fixedAuthTime.Add(365*24*time.Hour),
	); err != nil {
		t.Fatalf("insert admin token: %v", err)
	}

	sealer, err := idempotency.NewSealer(bytes.Repeat([]byte{0x42}, 32), bytes.NewReader(bytes.Repeat([]byte{0x24}, 1024)))
	if err != nil {
		t.Fatalf("NewSealer() error = %v", err)
	}
	idempotencyService := idempotency.NewService(pool, sealer, func() time.Time { return fixedAuthTime })
	service, err := NewService(ServiceOptions{
		Pool:           pool,
		Idempotency:    idempotencyService,
		Audit:          audit.NewService(pool),
		Pepper:         testPepper,
		Random:         bytes.NewReader(bytes.Repeat([]byte{0x51}, 1024)),
		IDs:            &sequenceGenerator{next: []uuid.UUID{uuid.MustParse("11111111-2222-4333-8444-555555555555")}},
		Clock:          clock.Fixed{Time: fixedAuthTime},
		IdempotencyTTL: 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	admin := Actor{
		TokenID:          tokenID,
		ServiceAccountID: serviceAccountID,
		Scopes:           NewScopeSet(ScopeAdmin),
		RepositoryIDs:    map[uuid.UUID]struct{}{},
	}
	return pool, service, admin, issued.Bearer
}

func fixedMutation(actor Actor, resource, key string) Mutation {
	return Mutation{
		Actor:             actor,
		RequestID:         "request-123",
		IdempotencyKey:    key,
		Fingerprint:       bytes.Repeat([]byte{0x11}, 32),
		CanonicalResource: resource,
	}
}

type sequenceGenerator struct {
	next []uuid.UUID
}

func (g *sequenceGenerator) New() uuid.UUID {
	value := g.next[0]
	g.next = g.next[1:]
	return value
}
