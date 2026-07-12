package repository

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"superfan.myasustor.com/fanchao/artifact-repository/internal/audit"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/auth"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/database"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/idempotency"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/testenv"
)

func TestCreateCommitsAuditAndReplaysOneRepository(t *testing.T) {
	pool, service, admin := newRepositoryTestService(t)
	request := CreateRequest{
		Mutation:    repositoryMutation(admin, "create-releases"),
		Key:         "releases",
		DisplayName: "Releases",
	}

	first, err := service.Create(t.Context(), request)
	if err != nil {
		t.Fatalf("first Create() error = %v", err)
	}
	second, err := service.Create(t.Context(), request)
	if err != nil {
		t.Fatalf("replay Create() error = %v", err)
	}
	if first.ID != second.ID || first.Key != "releases" {
		t.Fatalf("first = %+v, replay = %+v", first, second)
	}
	if first.Replayed || !second.Replayed {
		t.Fatalf("first replayed = %v, second replayed = %v", first.Replayed, second.Replayed)
	}

	var repositories, events int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM repositories WHERE key = 'releases'").Scan(&repositories); err != nil {
		t.Fatalf("count repositories: %v", err)
	}
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM audit_events WHERE action = 'repository.create'").Scan(&events); err != nil {
		t.Fatalf("count audit events: %v", err)
	}
	if repositories != 1 || events != 1 {
		t.Fatalf("repositories = %d, events = %d, want 1 and 1", repositories, events)
	}
}

func TestCreateConflictCompletesAndReplaysDeterministicResponse(t *testing.T) {
	pool, service, admin := newRepositoryTestService(t)
	if _, err := service.Create(t.Context(), CreateRequest{
		Mutation: repositoryMutation(admin, "create-existing"), Key: "existing", DisplayName: "Existing",
	}); err != nil {
		t.Fatalf("seed Create() error = %v", err)
	}
	request := CreateRequest{
		Mutation: repositoryMutation(admin, "create-conflict"), Key: "existing", DisplayName: "Duplicate",
	}

	_, err := service.Create(t.Context(), request)
	completed, ok := idempotency.CompletedErrorFrom(err)
	if !ok || completed.Replayed || completed.Status != 409 || !errors.Is(err, ErrConflict) {
		t.Fatalf("Create() conflict = %+v, err = %v", completed, err)
	}
	_, err = service.Create(t.Context(), request)
	replayed, ok := idempotency.CompletedErrorFrom(err)
	if !ok || !replayed.Replayed || replayed.Status != 409 {
		t.Fatalf("Create() conflict replay = %+v, err = %v", replayed, err)
	}

	var records, failedAudits int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM idempotency_records WHERE idempotency_key = 'create-conflict' AND state = 'completed' AND http_status = 409").Scan(&records); err != nil {
		t.Fatalf("count conflict records: %v", err)
	}
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM audit_events WHERE request_id = 'request-repository' AND action = 'repository.create' AND outcome = 'failed' AND code = 'conflict'").Scan(&failedAudits); err != nil {
		t.Fatalf("count conflict audits: %v", err)
	}
	if records != 1 || failedAudits != 1 {
		t.Fatalf("conflict records = %d, failed audits = %d, want 1 and 1", records, failedAudits)
	}
}

func TestListAndGetOnlyExposeAllowedRepositories(t *testing.T) {
	pool, service, admin := newRepositoryTestService(t)
	repositoryIDs := map[string]uuid.UUID{
		"alpha": uuid.MustParse("11111111-2222-4333-8444-555555555555"),
		"beta":  uuid.MustParse("22222222-3333-4444-8555-666666666666"),
		"gamma": uuid.MustParse("33333333-4444-4555-8666-777777777777"),
	}
	for key, id := range repositoryIDs {
		if _, err := pool.Exec(
			t.Context(),
			"INSERT INTO repositories (id, key, display_name, created_by) VALUES ($1, $2, $3, $4)",
			id, key, key, admin.TokenID,
		); err != nil {
			t.Fatalf("insert repository %s: %v", key, err)
		}
	}

	actor := auth.Actor{
		TokenID:          uuid.MustParse("44444444-5555-4666-8777-888888888888"),
		ServiceAccountID: uuid.MustParse("55555555-6666-4777-8888-999999999999"),
		Scopes:           auth.NewScopeSet(auth.ScopeReleasePublish),
		RepositoryIDs: map[uuid.UUID]struct{}{
			repositoryIDs["alpha"]: {},
			repositoryIDs["gamma"]: {},
		},
	}
	first, err := service.List(t.Context(), actor, ListRequest{Limit: 1})
	if err != nil {
		t.Fatalf("List() first page error = %v", err)
	}
	second, err := service.List(t.Context(), actor, ListRequest{Limit: 1, After: first.Next})
	if err != nil {
		t.Fatalf("List() second page error = %v", err)
	}
	if len(first.Items) != 1 || first.Items[0].Key != "alpha" || first.Next == nil {
		t.Fatalf("first page = %+v", first)
	}
	if len(second.Items) != 1 || second.Items[0].Key != "gamma" || second.Next != nil {
		t.Fatalf("second page = %+v", second)
	}
	if _, err := service.Get(t.Context(), actor, "beta"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(hidden) error = %v, want ErrNotFound", err)
	}
	visible, err := service.Get(t.Context(), actor, "alpha")
	if err != nil || visible.ID != repositoryIDs["alpha"] {
		t.Fatalf("Get(visible) = %+v, %v", visible, err)
	}

	emptyActor := actor
	emptyActor.RepositoryIDs = map[uuid.UUID]struct{}{}
	empty, err := service.List(t.Context(), emptyActor, ListRequest{})
	if err != nil {
		t.Fatalf("List(empty allowlist) error = %v", err)
	}
	if len(empty.Items) != 0 {
		t.Fatalf("List(empty allowlist) = %+v", empty)
	}
}

var repositoryTestTime = time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)

func newRepositoryTestService(t *testing.T) (*pgxpool.Pool, *Service, auth.Actor) {
	t.Helper()
	pool := testenv.StartPostgres(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	serviceAccountID := uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	tokenID := uuid.MustParse("bbbbbbbb-cccc-4ddd-8eee-ffffffffffff")
	if _, err := pool.Exec(t.Context(), "INSERT INTO service_accounts (id, name) VALUES ($1, 'repository-admin')", serviceAccountID); err != nil {
		t.Fatalf("insert service account: %v", err)
	}
	if _, err := pool.Exec(
		t.Context(),
		`INSERT INTO api_tokens
		 (id, service_account_id, secret_hmac, scopes, repository_ids, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		tokenID,
		serviceAccountID,
		bytes.Repeat([]byte{1}, 32),
		[]string{"admin"},
		[]uuid.UUID{},
		repositoryTestTime.Add(365*24*time.Hour),
	); err != nil {
		t.Fatalf("insert token: %v", err)
	}
	sealer, err := idempotency.NewSealer(bytes.Repeat([]byte{0x42}, 32), bytes.NewReader(bytes.Repeat([]byte{0x24}, 256)))
	if err != nil {
		t.Fatalf("NewSealer() error = %v", err)
	}
	service, err := NewService(Options{
		Pool:           pool,
		Idempotency:    idempotency.NewService(pool, sealer, func() time.Time { return repositoryTestTime }),
		Audit:          audit.NewService(pool),
		IdempotencyTTL: 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	admin := auth.Actor{
		TokenID:          tokenID,
		ServiceAccountID: serviceAccountID,
		Scopes:           auth.NewScopeSet(auth.ScopeAdmin),
		RepositoryIDs:    map[uuid.UUID]struct{}{},
	}
	return pool, service, admin
}

func repositoryMutation(actor auth.Actor, key string) auth.Mutation {
	return auth.Mutation{
		Actor:             actor,
		RequestID:         "request-repository",
		IdempotencyKey:    key,
		Fingerprint:       bytes.Repeat([]byte{0x11}, 32),
		CanonicalResource: "/api/v1/repositories",
	}
}
