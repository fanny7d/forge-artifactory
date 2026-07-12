package channel

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"superfan.myasustor.com/fanchao/artifact-repository/internal/audit"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/auth"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/blob"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/database"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/idempotency"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/storage"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/testenv"
)

func TestPromoteRejectsPublishedReleaseFromAnotherPackage(t *testing.T) {
	fixture := newChannelFixture(t)
	request := PromoteRequest{
		Mutation: auth.Mutation{
			Actor:             fixture.actor,
			Method:            "POST",
			RequestID:         "request-cross-package",
			IdempotencyKey:    "promote-cross-package",
			Fingerprint:       bytes.Repeat([]byte{0x31}, 32),
			CanonicalResource: "/api/v1/repositories/repo-a/packages/package-a/channels/candidate/promotions",
		},
		RepositoryKey: "repo-a",
		PackageName:   "package-a",
		ChannelName:   "candidate",
		Version:       "2.0.0",
		Reason:        "cross-package test",
	}
	_, err := fixture.service.Promote(t.Context(), request)
	if !errors.Is(err, ErrReleaseNotInPackage) {
		t.Fatalf("Promote() error = %v, want ErrReleaseNotInPackage", err)
	}
	completed, ok := idempotency.CompletedErrorFrom(err)
	if !ok || completed.Replayed || completed.Status != 422 {
		t.Fatalf("Promote() completed error = %+v, err = %v", completed, err)
	}
	_, err = fixture.service.Promote(t.Context(), request)
	replayed, ok := idempotency.CompletedErrorFrom(err)
	if !ok || !replayed.Replayed || replayed.Status != 422 {
		t.Fatalf("Promote() replay error = %+v, err = %v", replayed, err)
	}
	var revisions int
	if err := fixture.pool.QueryRow(t.Context(), "SELECT count(*) FROM channel_revisions").Scan(&revisions); err != nil {
		t.Fatalf("count channel revisions: %v", err)
	}
	if revisions != 0 {
		t.Fatalf("channel revisions = %d, want 0", revisions)
	}
	var records, deniedAudits int
	if err := fixture.pool.QueryRow(t.Context(), "SELECT count(*) FROM idempotency_records WHERE idempotency_key = 'promote-cross-package' AND state = 'completed' AND http_status = 422").Scan(&records); err != nil {
		t.Fatalf("count promotion denial records: %v", err)
	}
	if err := fixture.pool.QueryRow(t.Context(), "SELECT count(*) FROM audit_events WHERE request_id = 'request-cross-package' AND action = 'channel.promote' AND outcome = 'denied' AND code = 'release-not-in-package'").Scan(&deniedAudits); err != nil {
		t.Fatalf("count promotion denial audits: %v", err)
	}
	if records != 1 || deniedAudits != 1 {
		t.Fatalf("promotion records = %d, denial audits = %d, want 1 and 1", records, deniedAudits)
	}
}

func TestPromotionUpdatesCurrentAndHistoryAtomically(t *testing.T) {
	fixture := newChannelFixture(t)
	if _, err := fixture.pool.Exec(t.Context(),
		`INSERT INTO releases (package_id, version, state, published_at, created_by)
		 SELECT p.id, '1.1.0', 'published', now(), $1
		 FROM packages p WHERE p.name = 'package-a'`, fixture.actor.TokenID,
	); err != nil {
		t.Fatalf("insert second package-a release: %v", err)
	}
	firstRequest := channelPromotionRequest(fixture.actor, "1.0.0", "promote-1.0.0")
	first, err := fixture.service.Promote(t.Context(), firstRequest)
	if err != nil {
		t.Fatalf("Promote(first) error = %v", err)
	}
	replayed, err := fixture.service.Promote(t.Context(), firstRequest)
	if err != nil {
		t.Fatalf("Promote(first replay) error = %v", err)
	}
	if first.ID != replayed.ID || first.Replayed || !replayed.Replayed {
		t.Fatalf("first = %+v, replayed = %+v", first, replayed)
	}
	second, err := fixture.service.Promote(t.Context(), channelPromotionRequest(fixture.actor, "1.1.0", "promote-1.1.0"))
	if err != nil {
		t.Fatalf("Promote(second) error = %v", err)
	}
	if second.FromVersion == nil || *second.FromVersion != "1.0.0" || second.ToVersion != "1.1.0" {
		t.Fatalf("second revision = %+v", second)
	}

	reader := fixture.actor
	reader.Scopes = auth.NewScopeSet(auth.ScopeArtifactRead)
	current, err := fixture.service.Current(t.Context(), reader, "repo-a", "package-a", "candidate")
	if err != nil {
		t.Fatalf("Current() error = %v", err)
	}
	if current.CurrentVersion == nil || *current.CurrentVersion != "1.1.0" {
		t.Fatalf("current channel = %+v", current)
	}
	firstPage, err := fixture.service.History(t.Context(), reader, "repo-a", "package-a", "candidate", HistoryRequest{Limit: 1})
	if err != nil {
		t.Fatalf("History(first) error = %v", err)
	}
	if len(firstPage.Items) != 1 || firstPage.Next == nil || firstPage.Items[0].ToVersion != "1.1.0" {
		t.Fatalf("first history page = %+v", firstPage)
	}
	secondPage, err := fixture.service.History(t.Context(), reader, "repo-a", "package-a", "candidate", HistoryRequest{Limit: 1, After: firstPage.Next})
	if err != nil {
		t.Fatalf("History(second) error = %v", err)
	}
	if len(secondPage.Items) != 1 || secondPage.Items[0].ID != first.ID || secondPage.Items[0].ToVersion != "1.0.0" {
		t.Fatalf("second history page = %+v", secondPage)
	}
}

func TestResolveReturnsExactSignedArtifactAndDownloadMode(t *testing.T) {
	fixture := newChannelFixture(t)
	manifest, signature := fixture.seedResolvableRelease(t)
	reader := fixture.actor
	reader.Scopes = auth.NewScopeSet(auth.ScopeArtifactRead)
	request := ResolveRequest{
		Actor:         reader,
		RepositoryKey: "repo-a",
		PackageName:   "package-a",
		ChannelName:   "candidate",
		OS:            "linux",
		Arch:          "arm64",
		Variant:       "",
		Role:          "binary",
	}
	resolved, err := fixture.service.Resolve(t.Context(), request)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved.Version != "1.0.0" || !bytes.Equal(resolved.Manifest, manifest) || !bytes.Equal(resolved.Signature, signature) {
		t.Fatalf("resolved signed release = %+v", resolved)
	}
	if resolved.Artifact.Path != "linux/arm64/edgecli" || resolved.Artifact.SHA256 == "" || resolved.Artifact.Size != 7 || resolved.DownloadURL == "" {
		t.Fatalf("resolved artifact = %+v", resolved)
	}
	if _, err := fixture.service.Resolve(t.Context(), ResolveRequest{
		Actor: reader, RepositoryKey: "repo-a", PackageName: "package-a", ChannelName: "candidate",
		OS: "linux", Arch: "arm64", Variant: "musl", Role: "binary",
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve(mismatched variant) error = %v, want ErrNotFound", err)
	}
	redirect := false
	request.Redirect = &redirect
	proxied, err := fixture.service.Resolve(t.Context(), request)
	if err != nil {
		t.Fatalf("Resolve(proxy) error = %v", err)
	}
	if proxied.DownloadURL != "/api/v1/repositories/repo-a/artifacts/linux/arm64/edgecli?redirect=false" {
		t.Fatalf("proxy download URL = %q", proxied.DownloadURL)
	}
}

func TestConcurrentPromotionsSerializeRevisionHistory(t *testing.T) {
	fixture := newChannelFixture(t)
	if _, err := fixture.pool.Exec(t.Context(),
		`INSERT INTO releases (package_id, version, state, published_at, created_by)
		 SELECT p.id, '1.1.0', 'published', now(), $1
		 FROM packages p WHERE p.name = 'package-a'`, fixture.actor.TokenID,
	); err != nil {
		t.Fatalf("insert concurrent target: %v", err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, version := range []string{"1.0.0", "1.1.0"} {
		version := version
		go func() {
			<-start
			_, err := fixture.service.Promote(t.Context(), channelPromotionRequest(fixture.actor, version, "concurrent-"+version))
			results <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("concurrent Promote() error = %v", err)
		}
	}

	var revisions, initialRevisions int
	if err := fixture.pool.QueryRow(t.Context(),
		`SELECT count(*), count(*) FILTER (WHERE from_release_id IS NULL)
		 FROM channel_revisions`,
	).Scan(&revisions, &initialRevisions); err != nil {
		t.Fatalf("count concurrent revisions: %v", err)
	}
	if revisions != 2 || initialRevisions != 1 {
		t.Fatalf("concurrent history = revisions %d initial %d", revisions, initialRevisions)
	}
	var currentReleaseID, latestTargetID uuid.UUID
	if err := fixture.pool.QueryRow(t.Context(),
		`SELECT c.current_release_id,
		        (SELECT cr.to_release_id FROM channel_revisions cr WHERE cr.channel_id = c.id ORDER BY cr.created_at DESC, cr.id DESC LIMIT 1)
		 FROM channels c
		 JOIN packages p ON p.id = c.package_id
		 WHERE p.name = 'package-a' AND c.name = 'candidate'`,
	).Scan(&currentReleaseID, &latestTargetID); err != nil {
		t.Fatalf("load concurrent current release: %v", err)
	}
	if currentReleaseID != latestTargetID {
		t.Fatalf("current release = %s, latest target = %s", currentReleaseID, latestTargetID)
	}
}

func TestResolveFallsBackToProxyOnlyWhenRedirectIsImplicit(t *testing.T) {
	fixture := newChannelFixture(t)
	fixture.seedResolvableRelease(t)
	fixture.store.presignErr = storage.ErrPublicEndpointUnavailable
	reader := fixture.actor
	reader.Scopes = auth.NewScopeSet(auth.ScopeArtifactRead)
	request := ResolveRequest{
		Actor: reader, RepositoryKey: "repo-a", PackageName: "package-a", ChannelName: "candidate",
		OS: "linux", Arch: "arm64", Role: "binary",
	}
	resolved, err := fixture.service.Resolve(t.Context(), request)
	if err != nil {
		t.Fatalf("Resolve(default redirect) error = %v", err)
	}
	if resolved.DownloadURL != "/api/v1/repositories/repo-a/artifacts/linux/arm64/edgecli?redirect=false" {
		t.Fatalf("fallback download URL = %q", resolved.DownloadURL)
	}
	redirect := true
	request.Redirect = &redirect
	if _, err := fixture.service.Resolve(t.Context(), request); !errors.Is(err, ErrPublicEndpointUnavailable) {
		t.Fatalf("Resolve(explicit redirect) error = %v, want ErrPublicEndpointUnavailable", err)
	}
}

func channelPromotionRequest(actor auth.Actor, version, key string) PromoteRequest {
	return PromoteRequest{
		Mutation: auth.Mutation{
			Actor:             actor,
			Method:            "POST",
			RequestID:         "request-" + key,
			IdempotencyKey:    key,
			Fingerprint:       bytes.Repeat([]byte{0x32}, 32),
			CanonicalResource: "/api/v1/repositories/repo-a/packages/package-a/channels/candidate/promotions",
		},
		RepositoryKey: "repo-a",
		PackageName:   "package-a",
		ChannelName:   "candidate",
		Version:       version,
		Reason:        "promote " + version,
	}
}

type channelFixture struct {
	pool    *pgxpool.Pool
	service *Service
	actor   auth.Actor
	store   *channelMemoryStore
}

func newChannelFixture(t *testing.T) channelFixture {
	t.Helper()
	pool := testenv.StartPostgres(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	serviceAccountID := uuid.MustParse("aaaaaaaa-1111-4222-8333-bbbbbbbbbbbb")
	tokenID := uuid.MustParse("bbbbbbbb-2222-4333-8444-cccccccccccc")
	repositoryA := uuid.MustParse("11111111-aaaa-4bbb-8ccc-222222222222")
	repositoryB := uuid.MustParse("22222222-bbbb-4ccc-8ddd-333333333333")
	packageA := uuid.MustParse("33333333-cccc-4ddd-8eee-444444444444")
	packageB := uuid.MustParse("44444444-dddd-4eee-8fff-555555555555")
	if _, err := pool.Exec(t.Context(), "INSERT INTO service_accounts (id, name) VALUES ($1, 'channel-promoter')", serviceAccountID); err != nil {
		t.Fatalf("insert service account: %v", err)
	}
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO api_tokens
		 (id, service_account_id, secret_hmac, scopes, repository_ids, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		tokenID, serviceAccountID, bytes.Repeat([]byte{0x11}, 32), []string{"channel:promote"},
		[]uuid.UUID{repositoryA, repositoryB}, time.Now().UTC().Add(24*time.Hour),
	); err != nil {
		t.Fatalf("insert token: %v", err)
	}
	if _, err := pool.Exec(t.Context(),
		"INSERT INTO repositories (id, key, display_name, created_by) VALUES ($1, 'repo-a', 'Repo A', $3), ($2, 'repo-b', 'Repo B', $3)",
		repositoryA, repositoryB, tokenID,
	); err != nil {
		t.Fatalf("insert repositories: %v", err)
	}
	if _, err := pool.Exec(t.Context(),
		"INSERT INTO packages (id, repository_id, name, created_by) VALUES ($1, $2, 'package-a', $5), ($3, $4, 'package-b', $5)",
		packageA, repositoryA, packageB, repositoryB, tokenID,
	); err != nil {
		t.Fatalf("insert packages: %v", err)
	}
	if _, err := pool.Exec(t.Context(),
		"INSERT INTO channels (package_id, name) VALUES ($1, 'candidate'), ($1, 'stable'), ($2, 'candidate'), ($2, 'stable')",
		packageA, packageB,
	); err != nil {
		t.Fatalf("insert channels: %v", err)
	}
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO releases (package_id, version, state, published_at, created_by)
		 VALUES ($1, '1.0.0', 'published', now(), $3),
		        ($2, '2.0.0', 'published', now(), $3)`,
		packageA, packageB, tokenID,
	); err != nil {
		t.Fatalf("insert releases: %v", err)
	}
	sealer, err := idempotency.NewSealer(bytes.Repeat([]byte{0x41}, 32), bytes.NewReader(bytes.Repeat([]byte{0x42}, 1024)))
	if err != nil {
		t.Fatalf("NewSealer() error = %v", err)
	}
	store := &channelMemoryStore{objects: make(map[string][]byte)}
	service, err := NewService(Options{
		Pool:           pool,
		Idempotency:    idempotency.NewService(pool, sealer, func() time.Time { return time.Now().UTC() }),
		Audit:          audit.NewService(pool),
		Store:          store,
		IdempotencyTTL: 24 * time.Hour,
		PresignTTL:     15 * time.Minute,
	})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	return channelFixture{
		pool:    pool,
		service: service,
		store:   store,
		actor: auth.Actor{
			TokenID: tokenID,
			Scopes:  auth.NewScopeSet(auth.ScopeChannelPromote),
			RepositoryIDs: map[uuid.UUID]struct{}{
				repositoryA: {},
				repositoryB: {},
			},
		},
	}
}

func (f channelFixture) seedResolvableRelease(t *testing.T) ([]byte, []byte) {
	t.Helper()
	var repositoryID, packageID, releaseID uuid.UUID
	if err := f.pool.QueryRow(t.Context(), "SELECT id FROM repositories WHERE key = 'repo-a'").Scan(&repositoryID); err != nil {
		t.Fatalf("load repository: %v", err)
	}
	if err := f.pool.QueryRow(t.Context(), "SELECT id FROM packages WHERE repository_id = $1 AND name = 'package-a'", repositoryID).Scan(&packageID); err != nil {
		t.Fatalf("load package: %v", err)
	}
	if err := f.pool.QueryRow(t.Context(), "SELECT id FROM releases WHERE package_id = $1 AND version = '1.0.0'", packageID).Scan(&releaseID); err != nil {
		t.Fatalf("load release: %v", err)
	}
	artifactContent := []byte("edgecli")
	artifactDigest := sha256.Sum256(artifactContent)
	artifactSHA := hex.EncodeToString(artifactDigest[:])
	f.store.objects[blob.ObjectKey(artifactSHA)] = artifactContent
	if _, err := f.pool.Exec(t.Context(),
		"INSERT INTO blobs (sha256, size, object_key, state) VALUES ($1, $2, $3, 'ready')",
		artifactSHA, len(artifactContent), blob.ObjectKey(artifactSHA),
	); err != nil {
		t.Fatalf("insert artifact blob: %v", err)
	}
	var artifactID uuid.UUID
	if err := f.pool.QueryRow(t.Context(),
		`INSERT INTO artifacts
		 (repository_id, logical_path, blob_sha256, media_type, filename, properties, created_by)
		 VALUES ($1, 'linux/arm64/edgecli', $2, 'application/octet-stream', 'edgecli', '{}', $3)
		 RETURNING id`, repositoryID, artifactSHA, f.actor.TokenID,
	).Scan(&artifactID); err != nil {
		t.Fatalf("insert artifact: %v", err)
	}
	if _, err := f.pool.Exec(t.Context(),
		`INSERT INTO release_artifacts (release_id, artifact_id, os, arch, variant, role)
		 VALUES ($1, $2, 'linux', 'arm64', '', 'binary')`, releaseID, artifactID,
	); err != nil {
		t.Fatalf("insert release artifact: %v", err)
	}

	manifest := []byte(`{"schemaVersion":1,"version":"1.0.0"}`)
	seed := sha256.Sum256([]byte("channel-resolve-key"))
	privateKey := ed25519.NewKeyFromSeed(seed[:])
	publicKey := privateKey.Public().(ed25519.PublicKey)
	signature := ed25519.Sign(privateKey, manifest)
	manifestSHA := hex.EncodeToString(sha256Bytes(manifest))
	signatureSHA := hex.EncodeToString(sha256Bytes(signature))
	f.store.objects[blob.ObjectKey(manifestSHA)] = manifest
	f.store.objects[blob.ObjectKey(signatureSHA)] = signature
	if _, err := f.pool.Exec(t.Context(),
		`INSERT INTO blobs (sha256, size, object_key, state)
		 VALUES ($1, $2, $3, 'ready'), ($4, $5, $6, 'ready')`,
		manifestSHA, len(manifest), blob.ObjectKey(manifestSHA), signatureSHA, len(signature), blob.ObjectKey(signatureSHA),
	); err != nil {
		t.Fatalf("insert signed blobs: %v", err)
	}
	publicDigest := sha256.Sum256(publicKey)
	fingerprint := hex.EncodeToString(publicDigest[:])
	keyID := "ed25519:" + fingerprint
	if _, err := f.pool.Exec(t.Context(),
		"INSERT INTO signing_keys (key_id, algorithm, public_key, fingerprint, active) VALUES ($1, 'Ed25519', $2, $3, true)",
		keyID, []byte(publicKey), fingerprint,
	); err != nil {
		t.Fatalf("insert signing key: %v", err)
	}
	attemptID := uuid.MustParse("99999999-aaaa-4bbb-8ccc-dddddddddddd")
	owner := uuid.MustParse("88888888-9999-4aaa-8bbb-cccccccccccc")
	if _, err := f.pool.Exec(t.Context(),
		`INSERT INTO publish_attempts
		 (id, release_id, actor_token_id, request_id, published_at, snapshot, snapshot_sha256,
		  key_id, lease_owner, lease_expires_at, state, storage_completed, manifest_sha256, signature_sha256)
		 VALUES ($1, $2, $3, 'request-resolve', now(), '{}', repeat('a', 64), $4, $5, now(),
		         'completed', true, $6, $7)`,
		attemptID, releaseID, f.actor.TokenID, keyID, owner, manifestSHA, signatureSHA,
	); err != nil {
		t.Fatalf("insert publish attempt: %v", err)
	}
	if _, err := f.pool.Exec(t.Context(),
		`INSERT INTO release_manifests
		 (release_id, attempt_id, manifest_blob_sha256, signature_blob_sha256, key_id)
		 VALUES ($1, $2, $3, $4, $5)`, releaseID, attemptID, manifestSHA, signatureSHA, keyID,
	); err != nil {
		t.Fatalf("insert release manifest: %v", err)
	}
	if _, err := f.pool.Exec(t.Context(),
		"UPDATE releases SET current_attempt_id = $1 WHERE id = $2", attemptID, releaseID,
	); err != nil {
		t.Fatalf("link release attempt: %v", err)
	}
	if _, err := f.pool.Exec(t.Context(),
		"UPDATE channels SET current_release_id = $1 WHERE package_id = $2 AND name = 'candidate'", releaseID, packageID,
	); err != nil {
		t.Fatalf("point candidate channel: %v", err)
	}
	return manifest, signature
}

func sha256Bytes(content []byte) []byte {
	digest := sha256.Sum256(content)
	return digest[:]
}

type channelMemoryStore struct {
	objects    map[string][]byte
	presignErr error
}

func (s *channelMemoryStore) PutStaging(_ context.Context, key string, reader io.Reader, size int64) error {
	content, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	if int64(len(content)) != size {
		return storage.ErrObjectConflict
	}
	s.objects[key] = content
	return nil
}

func (s *channelMemoryStore) Promote(_ context.Context, stagingKey, objectKey string, _ int64) error {
	content, ok := s.objects[stagingKey]
	if !ok {
		return storage.ErrNotFound
	}
	s.objects[objectKey] = content
	delete(s.objects, stagingKey)
	return nil
}

func (s *channelMemoryStore) Open(_ context.Context, key, _ string) (storage.Object, error) {
	content, ok := s.objects[key]
	if !ok {
		return storage.Object{}, storage.ErrNotFound
	}
	reader := bytes.NewReader(content)
	return storage.Object{Body: io.NopCloser(reader), Seeker: reader, Info: storage.ObjectInfo{Key: key, Size: int64(len(content))}}, nil
}

func (s *channelMemoryStore) Stat(_ context.Context, key string) (storage.ObjectInfo, error) {
	content, ok := s.objects[key]
	if !ok {
		return storage.ObjectInfo{}, storage.ErrNotFound
	}
	return storage.ObjectInfo{Key: key, Size: int64(len(content))}, nil
}

func (*channelMemoryStore) List(context.Context, storage.ListRequest) (storage.ListPage, error) {
	return storage.ListPage{}, nil
}

func (s *channelMemoryStore) Delete(_ context.Context, key string) error {
	delete(s.objects, key)
	return nil
}

func (s *channelMemoryStore) Presign(_ context.Context, key string, _ time.Duration) (string, error) {
	if s.presignErr != nil {
		return "", s.presignErr
	}
	return "https://downloads.example.test/" + key, nil
}

func (*channelMemoryStore) Ready(context.Context) error { return nil }
