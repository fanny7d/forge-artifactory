package release

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

func TestCreatePackageScopesNameToRepositoryAndCreatesChannelsAtomically(t *testing.T) {
	pool, service, actor, repositories := newPackageTestService(t)
	firstRequest := CreatePackageRequest{
		Mutation:      packageMutation(actor, "repo-a", "create-package-a"),
		RepositoryKey: "repo-a",
		Name:          "edgecli",
	}
	first, err := service.Create(t.Context(), firstRequest)
	if err != nil {
		t.Fatalf("Create(repo-a) error = %v", err)
	}
	replayed, err := service.Create(t.Context(), firstRequest)
	if err != nil {
		t.Fatalf("Create(repo-a) replay error = %v", err)
	}
	second, err := service.Create(t.Context(), CreatePackageRequest{
		Mutation:      packageMutation(actor, "repo-b", "create-package-b"),
		RepositoryKey: "repo-b",
		Name:          "edgecli",
	})
	if err != nil {
		t.Fatalf("Create(repo-b) error = %v", err)
	}
	if first.ID == second.ID || first.RepositoryID != repositories["repo-a"] || second.RepositoryID != repositories["repo-b"] {
		t.Fatalf("repo-a package = %+v, repo-b package = %+v", first, second)
	}
	if first.Replayed || !replayed.Replayed || replayed.ID != first.ID {
		t.Fatalf("first = %+v, replay = %+v", first, replayed)
	}
	if !equalStrings(first.Channels, []string{"candidate", "stable"}) || !equalStrings(second.Channels, []string{"candidate", "stable"}) {
		t.Fatalf("channels = %#v, %#v", first.Channels, second.Channels)
	}

	var packages, channels, events int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM packages WHERE name = 'edgecli'").Scan(&packages); err != nil {
		t.Fatalf("count packages: %v", err)
	}
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM channels").Scan(&channels); err != nil {
		t.Fatalf("count channels: %v", err)
	}
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM audit_events WHERE action = 'package.create'").Scan(&events); err != nil {
		t.Fatalf("count audit events: %v", err)
	}
	if packages != 2 || channels != 4 || events != 2 {
		t.Fatalf("packages = %d, channels = %d, events = %d", packages, channels, events)
	}
}

func TestPackageReadsRequireArtifactReadAndRepositoryVisibility(t *testing.T) {
	_, service, publisher, repositories := newPackageTestService(t)
	for _, repositoryKey := range []string{"repo-a", "repo-b"} {
		if _, err := service.Create(t.Context(), CreatePackageRequest{
			Mutation:      packageMutation(publisher, repositoryKey, "create-"+repositoryKey),
			RepositoryKey: repositoryKey,
			Name:          "edgecli",
		}); err != nil {
			t.Fatalf("Create(%s) error = %v", repositoryKey, err)
		}
	}

	reader := auth.Actor{
		TokenID:          publisher.TokenID,
		ServiceAccountID: publisher.ServiceAccountID,
		Scopes:           auth.NewScopeSet(auth.ScopeArtifactRead),
		RepositoryIDs:    map[uuid.UUID]struct{}{repositories["repo-a"]: {}},
	}
	visible, err := service.Get(t.Context(), reader, "repo-a", "edgecli")
	if err != nil || visible.RepositoryKey != "repo-a" {
		t.Fatalf("Get(visible) = %+v, %v", visible, err)
	}
	if _, err := service.Get(t.Context(), reader, "repo-b", "edgecli"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(hidden) error = %v, want ErrNotFound", err)
	}
	page, err := service.List(t.Context(), reader, "repo-a", PackageListRequest{Limit: 1})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Name != "edgecli" {
		t.Fatalf("List() page = %+v", page)
	}
	publisherOnly := reader
	publisherOnly.Scopes = auth.NewScopeSet(auth.ScopeReleasePublish)
	if _, err := service.Get(t.Context(), publisherOnly, "repo-a", "edgecli"); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("Get() with release:publish error = %v, want ErrForbidden", err)
	}
}

var packageTestTime = time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)

func newPackageTestService(t *testing.T) (*pgxpool.Pool, *PackageService, auth.Actor, map[string]uuid.UUID) {
	t.Helper()
	pool := testenv.StartPostgres(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	serviceAccountID := uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	tokenID := uuid.MustParse("bbbbbbbb-cccc-4ddd-8eee-ffffffffffff")
	repositories := map[string]uuid.UUID{
		"repo-a": uuid.MustParse("11111111-2222-4333-8444-555555555555"),
		"repo-b": uuid.MustParse("22222222-3333-4444-8555-666666666666"),
	}
	if _, err := pool.Exec(t.Context(), "INSERT INTO service_accounts (id, name) VALUES ($1, 'package-publisher')", serviceAccountID); err != nil {
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
		[]string{"release:publish"},
		[]uuid.UUID{repositories["repo-a"], repositories["repo-b"]},
		packageTestTime.Add(365*24*time.Hour),
	); err != nil {
		t.Fatalf("insert API token: %v", err)
	}
	for key, repositoryID := range repositories {
		if _, err := pool.Exec(t.Context(), "INSERT INTO repositories (id, key, display_name, created_by) VALUES ($1, $2, $2, $3)", repositoryID, key, tokenID); err != nil {
			t.Fatalf("insert repository %s: %v", key, err)
		}
	}
	sealer, err := idempotency.NewSealer(bytes.Repeat([]byte{0x42}, 32), bytes.NewReader(bytes.Repeat([]byte{0x24}, 512)))
	if err != nil {
		t.Fatalf("NewSealer() error = %v", err)
	}
	service, err := NewPackageService(PackageServiceOptions{
		Pool:           pool,
		Idempotency:    idempotency.NewService(pool, sealer, func() time.Time { return packageTestTime }),
		Audit:          audit.NewService(pool),
		IdempotencyTTL: 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewPackageService() error = %v", err)
	}
	actor := auth.Actor{
		TokenID:          tokenID,
		ServiceAccountID: serviceAccountID,
		Scopes:           auth.NewScopeSet(auth.ScopeReleasePublish),
		RepositoryIDs: map[uuid.UUID]struct{}{
			repositories["repo-a"]: {},
			repositories["repo-b"]: {},
		},
	}
	return pool, service, actor, repositories
}

func packageMutation(actor auth.Actor, repositoryKey, key string) auth.Mutation {
	return auth.Mutation{
		Actor:             actor,
		RequestID:         "request-package",
		IdempotencyKey:    key,
		Fingerprint:       bytes.Repeat([]byte{0x11}, 32),
		CanonicalResource: "/api/v1/repositories/" + repositoryKey + "/packages",
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
