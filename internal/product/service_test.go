package product

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

func TestCreateEnsuresProductResourcesAndRotateInstallKeyReplays(t *testing.T) {
	pool, service, admin := newProductTestService(t)
	request := CreateRequest{
		Mutation:    productMutation(admin, "create-edgectl", "/api/v1/products"),
		Slug:        "edgectl",
		DisplayName: "Edge CLI",
		Description: "Company edge management CLI",
		CommandName: "edgectl",
	}

	first, err := service.Create(t.Context(), request)
	if err != nil {
		t.Fatalf("first Create() error = %v", err)
	}
	second, err := service.Create(t.Context(), request)
	if err != nil {
		t.Fatalf("replay Create() error = %v", err)
	}
	if first.ID != second.ID || first.InstallKey != second.InstallKey {
		t.Fatalf("first = %+v, replay = %+v", first, second)
	}
	if first.InstallKey == uuid.Nil || first.Replayed || !second.Replayed {
		t.Fatalf("first replayed/key = %v/%s, second replayed = %v", first.Replayed, first.InstallKey, second.Replayed)
	}
	if first.RepositoryKey != CLIRepositoryKey || first.PackageName != "edgectl" {
		t.Fatalf("created product = %+v", first)
	}
	if first.CurrentStableVersion != nil || first.CurrentStablePublishedAt != nil || len(first.Platforms) != 0 {
		t.Fatalf("new product stable summary = version %v, published %v, platforms %+v", first.CurrentStableVersion, first.CurrentStablePublishedAt, first.Platforms)
	}

	var repositories, packages, channels, products, successfulAudits, encryptedResponses int
	assertCount(t, pool, &repositories, "SELECT count(*) FROM repositories WHERE key = 'cli-releases'")
	assertCount(t, pool, &packages, "SELECT count(*) FROM packages WHERE repository_id = $1 AND name = 'edgectl'", first.RepositoryID)
	assertCount(t, pool, &channels, "SELECT count(*) FROM channels WHERE package_id = $1 AND name IN ('candidate', 'stable')", first.PackageID)
	assertCount(t, pool, &products, "SELECT count(*) FROM products WHERE slug = 'edgectl'")
	assertCount(t, pool, &successfulAudits, "SELECT count(*) FROM audit_events WHERE action = 'product.create' AND outcome = 'success'")
	assertCount(t, pool, &encryptedResponses, "SELECT count(*) FROM idempotency_records WHERE idempotency_key = 'create-edgectl' AND response_encrypted")
	if repositories != 1 || packages != 1 || channels != 2 || products != 1 || successfulAudits != 1 || encryptedResponses != 1 {
		t.Fatalf(
			"repositories/packages/channels/products/audits/encrypted = %d/%d/%d/%d/%d/%d",
			repositories, packages, channels, products, successfulAudits, encryptedResponses,
		)
	}

	byPackage, err := service.GetByPackageID(t.Context(), first.PackageID)
	if err != nil || byPackage.ID != first.ID || byPackage.CommandName != "edgectl" {
		t.Fatalf("GetByPackageID() = %+v, %v", byPackage, err)
	}
	byInstallKey, err := service.GetByInstallKey(t.Context(), first.InstallKey)
	if err != nil || byInstallKey.ID != first.ID {
		t.Fatalf("GetByInstallKey() = %+v, %v", byInstallKey, err)
	}

	rotateRequest := RotateInstallKeyRequest{
		Mutation: productMutation(admin, "rotate-edgectl", "/api/v1/products/edgectl/install-key"),
		Slug:     "edgectl",
	}
	rotated, err := service.RotateInstallKey(t.Context(), rotateRequest)
	if err != nil {
		t.Fatalf("RotateInstallKey() error = %v", err)
	}
	replayed, err := service.RotateInstallKey(t.Context(), rotateRequest)
	if err != nil {
		t.Fatalf("RotateInstallKey() replay error = %v", err)
	}
	if rotated.InstallKey == first.InstallKey || rotated.InstallKey == uuid.Nil {
		t.Fatalf("rotated key = %s, old key = %s", rotated.InstallKey, first.InstallKey)
	}
	if replayed.InstallKey != rotated.InstallKey || rotated.Replayed || !replayed.Replayed {
		t.Fatalf("rotated = %+v, replayed = %+v", rotated, replayed)
	}
	if resolved, err := service.GetByInstallKey(t.Context(), first.InstallKey); err != nil || resolved.ID != first.ID {
		t.Fatalf("GetByInstallKey(previous during grace) = %+v, %v", resolved, err)
	}
	if resolved, err := service.GetByInstallKey(t.Context(), rotated.InstallKey); err != nil || resolved.ID != first.ID {
		t.Fatalf("GetByInstallKey(rotated) = %+v, %v", resolved, err)
	}
	if _, err := pool.Exec(
		t.Context(),
		"UPDATE products SET previous_install_key_expires_at = now() - interval '1 second' WHERE id = $1",
		first.ID,
	); err != nil {
		t.Fatalf("expire previous install key: %v", err)
	}
	if _, err := service.GetByInstallKey(t.Context(), first.InstallKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByInstallKey(expired previous) error = %v, want ErrNotFound", err)
	}
}

func TestCreateRequiresAdminAndLeavesNoResources(t *testing.T) {
	pool, service, admin := newProductTestService(t)
	reader := admin
	reader.Scopes = auth.NewScopeSet(auth.ScopeArtifactRead)

	_, err := service.Create(t.Context(), CreateRequest{
		Mutation:    productMutation(reader, "create-denied", "/api/v1/products"),
		Slug:        "private-cli",
		DisplayName: "Private CLI",
		CommandName: "private-cli",
	})
	if !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("Create() error = %v, want auth.ErrForbidden", err)
	}
	var repositories, packages, products int
	assertCount(t, pool, &repositories, "SELECT count(*) FROM repositories WHERE key = 'cli-releases'")
	assertCount(t, pool, &packages, "SELECT count(*) FROM packages")
	assertCount(t, pool, &products, "SELECT count(*) FROM products")
	if repositories != 0 || packages != 0 || products != 0 {
		t.Fatalf("repositories/packages/products = %d/%d/%d, want 0/0/0", repositories, packages, products)
	}
}

func TestListAndGetRequireAdminAndSummarizeStableRelease(t *testing.T) {
	pool, service, admin := newProductTestService(t)
	edgectl := createTestProduct(t, service, admin, "edgectl")
	_ = createTestProduct(t, service, admin, "forge-cli")
	publishedAt := time.Date(2026, 7, 23, 8, 30, 0, 0, time.UTC)
	seedStableRelease(t, pool, admin.TokenID, edgectl, publishedAt)

	reader := auth.Actor{
		TokenID:          uuid.MustParse("cccccccc-dddd-4eee-8fff-aaaaaaaaaaaa"),
		ServiceAccountID: uuid.MustParse("dddddddd-eeee-4fff-8aaa-bbbbbbbbbbbb"),
		Scopes:           auth.NewScopeSet(auth.ScopeArtifactRead),
		RepositoryIDs: map[uuid.UUID]struct{}{
			edgectl.RepositoryID: {},
		},
	}
	if _, err := service.List(t.Context(), reader, ListRequest{}); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("List(reader) error = %v, want auth.ErrForbidden", err)
	}
	if _, err := service.Get(t.Context(), reader, "edgectl"); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("Get(reader) error = %v, want auth.ErrForbidden", err)
	}

	pageOne, err := service.List(t.Context(), admin, ListRequest{Limit: 1})
	if err != nil {
		t.Fatalf("List(first) error = %v", err)
	}
	pageTwo, err := service.List(t.Context(), admin, ListRequest{Limit: 1, After: pageOne.Next})
	if err != nil {
		t.Fatalf("List(second) error = %v", err)
	}
	if len(pageOne.Items) != 1 || pageOne.Items[0].Slug != "edgectl" || pageOne.Next == nil {
		t.Fatalf("first page = %+v", pageOne)
	}
	if len(pageTwo.Items) != 1 || pageTwo.Items[0].Slug != "forge-cli" || pageTwo.Next != nil {
		t.Fatalf("second page = %+v", pageTwo)
	}

	visible, err := service.Get(t.Context(), admin, "edgectl")
	if err != nil {
		t.Fatalf("Get(visible) error = %v", err)
	}
	if visible.CurrentStableVersion == nil || *visible.CurrentStableVersion != "1.2.3" {
		t.Fatalf("stable version = %v", visible.CurrentStableVersion)
	}
	if visible.CurrentStablePublishedAt == nil || !visible.CurrentStablePublishedAt.Equal(publishedAt) {
		t.Fatalf("stable publishedAt = %v, want %s", visible.CurrentStablePublishedAt, publishedAt)
	}
	wantPlatforms := []Platform{
		{OS: "linux", Arch: "amd64", Variant: "", Strategy: "self-replace", Format: "raw"},
		{OS: "linux", Arch: "arm64", Variant: "", Strategy: "bundle", Format: "tar.gz"},
	}
	if len(visible.Platforms) != len(wantPlatforms) {
		t.Fatalf("platforms = %+v", visible.Platforms)
	}
	for index := range wantPlatforms {
		if visible.Platforms[index] != wantPlatforms[index] {
			t.Fatalf("platforms[%d] = %+v, want %+v", index, visible.Platforms[index], wantPlatforms[index])
		}
	}

	adminPage, err := service.List(t.Context(), admin, ListRequest{})
	if err != nil || len(adminPage.Items) != 2 {
		t.Fatalf("List(admin) = %+v, %v", adminPage, err)
	}
}

var productTestTime = time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)

func newProductTestService(t *testing.T) (*pgxpool.Pool, *Service, auth.Actor) {
	t.Helper()
	pool := testenv.StartPostgres(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	serviceAccountID := uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	tokenID := uuid.MustParse("bbbbbbbb-cccc-4ddd-8eee-ffffffffffff")
	if _, err := pool.Exec(t.Context(), "INSERT INTO service_accounts (id, name) VALUES ($1, 'product-admin')", serviceAccountID); err != nil {
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
		productTestTime.Add(365*24*time.Hour),
	); err != nil {
		t.Fatalf("insert token: %v", err)
	}
	sealer, err := idempotency.NewSealer(bytes.Repeat([]byte{0x42}, 32), bytes.NewReader(bytes.Repeat([]byte{0x24}, 2048)))
	if err != nil {
		t.Fatalf("NewSealer() error = %v", err)
	}
	service, err := NewService(Options{
		Pool:           pool,
		Idempotency:    idempotency.NewService(pool, sealer, func() time.Time { return productTestTime }),
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

func productMutation(actor auth.Actor, key, resource string) auth.Mutation {
	return auth.Mutation{
		Actor:             actor,
		RequestID:         "request-" + key,
		IdempotencyKey:    key,
		Fingerprint:       bytes.Repeat([]byte{0x11}, 32),
		CanonicalResource: resource,
	}
}

func createTestProduct(t *testing.T, service *Service, admin auth.Actor, slug string) Product {
	t.Helper()
	created, err := service.Create(t.Context(), CreateRequest{
		Mutation:    productMutation(admin, "create-"+slug, "/api/v1/products"),
		Slug:        slug,
		DisplayName: slug,
		CommandName: slug,
	})
	if err != nil {
		t.Fatalf("Create(%s) error = %v", slug, err)
	}
	return created
}

func seedStableRelease(
	t *testing.T,
	pool *pgxpool.Pool,
	actorTokenID uuid.UUID,
	product Product,
	publishedAt time.Time,
) {
	t.Helper()
	releaseID := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	if _, err := pool.Exec(
		t.Context(),
		`INSERT INTO releases (id, package_id, version, state, published_at, created_by)
		 VALUES ($1, $2, '1.2.3', 'published', $3, $4)`,
		releaseID,
		product.PackageID,
		publishedAt,
		actorTokenID,
	); err != nil {
		t.Fatalf("insert stable release: %v", err)
	}
	platforms := []struct {
		sha         string
		logicalPath string
		filename    string
		os          string
		arch        string
		installSpec string
	}{
		{
			sha:         "1111111111111111111111111111111111111111111111111111111111111111",
			logicalPath: "edgectl/1.2.3/linux-amd64",
			filename:    "edgectl",
			os:          "linux",
			arch:        "amd64",
			installSpec: `{"strategy":"self-replace","format":"raw","mode":"0755"}`,
		},
		{
			sha:         "2222222222222222222222222222222222222222222222222222222222222222",
			logicalPath: "edgectl/1.2.3/linux-arm64",
			filename:    "edgectl.tar.gz",
			os:          "linux",
			arch:        "arm64",
			installSpec: `{"strategy":"bundle","format":"tar.gz","entrypoint":"bin/edgectl","mode":"0755"}`,
		},
		{
			sha:         "3333333333333333333333333333333333333333333333333333333333333333",
			logicalPath: "edgectl/1.2.3/windows-amd64",
			filename:    "edgectl.exe",
			os:          "windows",
			arch:        "amd64",
			installSpec: `{}`,
		},
	}
	for _, platform := range platforms {
		if _, err := pool.Exec(
			t.Context(),
			"INSERT INTO blobs (sha256, size, object_key, state) VALUES ($1, 100, $2, 'ready')",
			platform.sha,
			"blobs/"+platform.sha,
		); err != nil {
			t.Fatalf("insert blob %s: %v", platform.os+"/"+platform.arch, err)
		}
		var artifactID uuid.UUID
		if err := pool.QueryRow(
			t.Context(),
			`INSERT INTO artifacts
			 (repository_id, logical_path, blob_sha256, media_type, filename, created_by)
			 VALUES ($1, $2, $3, 'application/octet-stream', $4, $5)
			 RETURNING id`,
			product.RepositoryID,
			platform.logicalPath,
			platform.sha,
			platform.filename,
			actorTokenID,
		).Scan(&artifactID); err != nil {
			t.Fatalf("insert artifact %s: %v", platform.os+"/"+platform.arch, err)
		}
		if _, err := pool.Exec(
			t.Context(),
			`INSERT INTO release_artifacts
			 (release_id, artifact_id, os, arch, variant, role, install_spec)
			 VALUES ($1, $2, $3, $4, '', 'binary', $5::jsonb)`,
			releaseID,
			artifactID,
			platform.os,
			platform.arch,
			platform.installSpec,
		); err != nil {
			t.Fatalf("insert release artifact %s: %v", platform.os+"/"+platform.arch, err)
		}
	}
	if _, err := pool.Exec(
		t.Context(),
		"UPDATE channels SET current_release_id = $1 WHERE package_id = $2 AND name = 'stable'",
		releaseID,
		product.PackageID,
	); err != nil {
		t.Fatalf("promote stable release: %v", err)
	}
}

func assertCount(t *testing.T, pool *pgxpool.Pool, destination *int, query string, arguments ...any) {
	t.Helper()
	if err := pool.QueryRow(t.Context(), query, arguments...).Scan(destination); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
}
