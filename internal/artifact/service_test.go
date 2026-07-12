package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"superfan.myasustor.com/fanchao/artifact-repository/internal/audit"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/auth"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/blob"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/clock"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/database"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/storage"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/testenv"
)

func TestUploadStreamsPromotesAndCreatesImmutableArtifact(t *testing.T) {
	_, service, store, actor, _ := newArtifactTestService(t)
	content := []byte("edgecli")
	digest := sha256.Sum256(content)

	created, err := service.Upload(t.Context(), UploadRequest{
		Actor:          actor,
		RequestID:      "request-upload",
		RepositoryKey:  "source",
		RawPath:        "linux/arm64/edgecli",
		Body:           bytes.NewReader(content),
		ContentLength:  int64(len(content)),
		MediaType:      "application/octet-stream",
		Properties:     map[string]any{"kind": "binary"},
		ExpectedSHA256: fmt.Sprintf("%x", digest),
	})
	if err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
	if created.Path != "linux/arm64/edgecli" || created.SHA256 != fmt.Sprintf("%x", digest) || created.Size != int64(len(content)) {
		t.Fatalf("created artifact = %+v", created)
	}
	if created.Properties["kind"] != "binary" || created.Filename != "edgecli" {
		t.Fatalf("created metadata = %+v", created)
	}
	if len(store.promotions) != 1 || !bytes.Equal(store.objects[blob.ObjectKey(created.SHA256)], content) {
		t.Fatalf("promotions = %#v, objects = %#v", store.promotions, store.objects)
	}

	metadata, err := service.Metadata(t.Context(), actor, "source", "linux/arm64/edgecli")
	if err != nil {
		t.Fatalf("Metadata() error = %v", err)
	}
	if metadata.ID != created.ID || metadata.Properties["kind"] != "binary" {
		t.Fatalf("Metadata() = %+v", metadata)
	}
	if _, err := service.Upload(t.Context(), UploadRequest{
		Actor: actor, RequestID: "request-upload-again", RepositoryKey: "source", RawPath: "linux/arm64/edgecli",
		Body: bytes.NewReader(content), ContentLength: int64(len(content)), MediaType: "application/octet-stream",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("second Upload() error = %v, want ErrConflict", err)
	}
}

func TestUploadReportsBlobDedupDecision(t *testing.T) {
	_, service, _, actor, _ := newArtifactTestService(t)
	observer := &blobDedupObserver{}
	service.metrics = observer
	content := []byte("same-content")
	for _, path := range []string{"dedup/first", "dedup/second"} {
		if _, err := service.Upload(t.Context(), UploadRequest{
			Actor: actor, RequestID: "request-" + path, RepositoryKey: "source", RawPath: path,
			Body: bytes.NewReader(content), ContentLength: int64(len(content)), MediaType: "application/octet-stream",
		}); err != nil {
			t.Fatalf("Upload(%s) error = %v", path, err)
		}
	}
	if len(observer.hits) != 2 || observer.hits[0] || !observer.hits[1] {
		t.Fatalf("dedup observations = %v, want [false true]", observer.hits)
	}
}

func TestDatabaseFailureAfterPromotionDoesNotExposeArtifact(t *testing.T) {
	pool, service, store, actor, _ := newArtifactTestService(t)
	if _, err := pool.Exec(t.Context(), `
		CREATE FUNCTION reject_test_artifact() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.logical_path = 'fail/after-promote' THEN
				RAISE EXCEPTION 'forced artifact insert failure';
			END IF;
			RETURN NEW;
		END;
		$$;
		CREATE TRIGGER reject_test_artifact
		BEFORE INSERT ON artifacts
		FOR EACH ROW EXECUTE FUNCTION reject_test_artifact();
	`); err != nil {
		t.Fatalf("install failure trigger: %v", err)
	}
	content := []byte("promoted-but-not-visible")

	_, err := service.Upload(t.Context(), UploadRequest{
		Actor: actor, RequestID: "request-failure", RepositoryKey: "source", RawPath: "fail/after-promote",
		Body: bytes.NewReader(content), ContentLength: int64(len(content)), MediaType: "application/octet-stream",
	})
	if err == nil {
		t.Fatal("Upload() succeeded despite database trigger")
	}
	if len(store.promotions) != 1 {
		t.Fatalf("promotions = %#v, want one completed promotion", store.promotions)
	}
	var artifacts int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM artifacts WHERE logical_path = 'fail/after-promote'").Scan(&artifacts); err != nil {
		t.Fatalf("count artifacts: %v", err)
	}
	if artifacts != 0 {
		t.Fatalf("visible artifacts = %d, want 0", artifacts)
	}
}

func TestChecksumDeployRequiresVisibleSourceRepository(t *testing.T) {
	_, service, _, actor, repositories := newArtifactTestService(t)
	content := []byte("deduplicated")
	digest := sha256.Sum256(content)
	sha := fmt.Sprintf("%x", digest)
	if _, err := service.Upload(t.Context(), UploadRequest{
		Actor: actor, RequestID: "request-source", RepositoryKey: "source", RawPath: "edgecli/source",
		Body: bytes.NewReader(content), ContentLength: int64(len(content)), MediaType: "application/octet-stream",
	}); err != nil {
		t.Fatalf("Upload(source) error = %v", err)
	}

	restricted := actor
	restricted.RepositoryIDs = map[uuid.UUID]struct{}{repositories["target"]: {}}
	request := ChecksumDeployRequest{
		Actor: restricted, RequestID: "request-deploy", RepositoryKey: "target", RawPath: "edgecli/target",
		SHA256: sha, MediaType: "application/octet-stream", Properties: map[string]any{"deployed": true},
	}
	if _, err := service.ChecksumDeploy(t.Context(), request); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ChecksumDeploy(hidden source) error = %v, want ErrNotFound", err)
	}

	request.Actor = actor
	deployed, err := service.ChecksumDeploy(t.Context(), request)
	if err != nil {
		t.Fatalf("ChecksumDeploy(visible source) error = %v", err)
	}
	if deployed.SHA256 != sha || deployed.RepositoryKey != "target" || deployed.Properties["deployed"] != true {
		t.Fatalf("deployed artifact = %+v", deployed)
	}
}

func TestOpenDefaultsToProxyWhenPublicEndpointIsUnavailable(t *testing.T) {
	_, service, store, actor, _ := newArtifactTestService(t)
	content := []byte("download")
	if _, err := service.Upload(t.Context(), UploadRequest{
		Actor: actor, RequestID: "request-download", RepositoryKey: "source", RawPath: "edgecli/download",
		Body: bytes.NewReader(content), ContentLength: int64(len(content)), MediaType: "application/octet-stream",
	}); err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
	store.presignErr = storage.ErrPublicEndpointUnavailable

	result, err := service.Open(t.Context(), OpenRequest{
		Actor: actor, RepositoryKey: "source", RawPath: "edgecli/download",
	})
	if err != nil {
		t.Fatalf("Open(default) error = %v", err)
	}
	if result.RedirectURL != "" || result.Object.Body == nil {
		t.Fatalf("Open(default) result = %+v", result)
	}
	opened, err := io.ReadAll(result.Object.Body)
	_ = result.Object.Body.Close()
	if err != nil || !bytes.Equal(opened, content) {
		t.Fatalf("proxy body = %q, error = %v", opened, err)
	}

	redirect := true
	if _, err := service.Open(t.Context(), OpenRequest{
		Actor: actor, RepositoryKey: "source", RawPath: "edgecli/download", Redirect: &redirect,
	}); !errors.Is(err, ErrPublicEndpointUnavailable) {
		t.Fatalf("Open(explicit redirect) error = %v, want ErrPublicEndpointUnavailable", err)
	}
}

func TestUploadEnforcesIdleTimeoutAndDeclaredLength(t *testing.T) {
	_, service, _, actor, _ := newArtifactTestService(t)
	service.uploadIdleTimeout = 10 * time.Millisecond
	_, err := service.Upload(t.Context(), UploadRequest{
		Actor: actor, RequestID: "request-idle", RepositoryKey: "source", RawPath: "idle/object",
		Body:          &slowReader{delay: 50 * time.Millisecond, content: []byte("x")},
		ContentLength: 1, MediaType: "application/octet-stream",
	})
	if !errors.Is(err, ErrUploadIdle) {
		t.Fatalf("Upload(idle) error = %v, want ErrUploadIdle", err)
	}

	_, err = service.Upload(t.Context(), UploadRequest{
		Actor: actor, RequestID: "request-overflow", RepositoryKey: "source", RawPath: "overflow/object",
		Body: bytes.NewReader([]byte("xy")), ContentLength: 1, MediaType: "application/octet-stream",
	})
	if !errors.Is(err, ErrLengthMismatch) {
		t.Fatalf("Upload(overflow) error = %v, want ErrLengthMismatch", err)
	}
}

func TestUploadHeartbeatsSessionWhileStreaming(t *testing.T) {
	pool, service, _, actor, _ := newArtifactTestService(t)
	now := &artifactMutableClock{value: artifactTestTime}
	service.clock = now
	content := []byte("heartbeat")
	reader := &advancingReader{reader: bytes.NewReader(content), clock: now, advance: 31 * time.Second}

	if _, err := service.Upload(t.Context(), UploadRequest{
		Actor: actor, RequestID: "request-heartbeat", RepositoryKey: "source", RawPath: "heartbeat/object",
		Body: reader, ContentLength: int64(len(content)), MediaType: "application/octet-stream",
	}); err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
	var lastHeartbeat time.Time
	if err := pool.QueryRow(t.Context(), "SELECT last_heartbeat_at FROM upload_sessions WHERE logical_path = 'heartbeat/object'").Scan(&lastHeartbeat); err != nil {
		t.Fatalf("read upload heartbeat: %v", err)
	}
	if !lastHeartbeat.After(artifactTestTime) {
		t.Fatalf("last heartbeat = %s, want after %s", lastHeartbeat, artifactTestTime)
	}
}

var artifactTestTime = time.Now().UTC().Truncate(time.Second)

func newArtifactTestService(t *testing.T) (*pgxpool.Pool, *Service, *memoryStore, auth.Actor, map[string]uuid.UUID) {
	t.Helper()
	pool := testenv.StartPostgres(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	serviceAccountID := uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	tokenID := uuid.MustParse("bbbbbbbb-cccc-4ddd-8eee-ffffffffffff")
	repositories := map[string]uuid.UUID{
		"source": uuid.MustParse("11111111-2222-4333-8444-555555555555"),
		"target": uuid.MustParse("22222222-3333-4444-8555-666666666666"),
	}
	if _, err := pool.Exec(t.Context(), "INSERT INTO service_accounts (id, name) VALUES ($1, 'artifact-writer')", serviceAccountID); err != nil {
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
		[]string{"artifact:read", "artifact:write"},
		[]uuid.UUID{repositories["source"], repositories["target"]},
		artifactTestTime.Add(365*24*time.Hour),
	); err != nil {
		t.Fatalf("insert token: %v", err)
	}
	for key, repositoryID := range repositories {
		if _, err := pool.Exec(t.Context(), "INSERT INTO repositories (id, key, display_name, created_by) VALUES ($1, $2, $2, $3)", repositoryID, key, tokenID); err != nil {
			t.Fatalf("insert repository %s: %v", key, err)
		}
	}
	blobService, err := blob.NewService(blob.Options{Pool: pool, Clock: clock.Fixed{Time: artifactTestTime}, Lease: 2 * time.Minute})
	if err != nil {
		t.Fatalf("new blob service: %v", err)
	}
	store := &memoryStore{objects: map[string][]byte{}}
	service, err := NewService(Options{
		Pool:              pool,
		Blobs:             blobService,
		Store:             store,
		Audit:             audit.NewService(pool),
		Clock:             clock.Fixed{Time: artifactTestTime},
		IDs:               &artifactIDGenerator{},
		MaxUploadBytes:    1 << 20,
		UploadIdleTimeout: 10 * time.Minute,
		UploadLease:       2 * time.Minute,
		UploadHeartbeat:   30 * time.Second,
		UploadMaxDuration: time.Hour,
		PresignTTL:        15 * time.Minute,
	})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	actor := auth.Actor{
		TokenID: tokenID, ServiceAccountID: serviceAccountID,
		Scopes: auth.NewScopeSet(auth.ScopeArtifactRead, auth.ScopeArtifactWrite),
		RepositoryIDs: map[uuid.UUID]struct{}{
			repositories["source"]: {}, repositories["target"]: {},
		},
	}
	return pool, service, store, actor, repositories
}

type artifactIDGenerator struct{}

func (*artifactIDGenerator) New() uuid.UUID {
	return uuid.New()
}

type blobDedupObserver struct {
	hits []bool
}

func (o *blobDedupObserver) ObserveBlobDedup(hit bool) {
	o.hits = append(o.hits, hit)
}

type slowReader struct {
	delay   time.Duration
	content []byte
	done    bool
}

type artifactMutableClock struct {
	value time.Time
}

func (c *artifactMutableClock) Now() time.Time { return c.value }

type advancingReader struct {
	reader  io.Reader
	clock   *artifactMutableClock
	advance time.Duration
}

func (r *advancingReader) Read(buffer []byte) (int, error) {
	count, err := r.reader.Read(buffer)
	if count > 0 {
		r.clock.value = r.clock.value.Add(r.advance)
	}
	return count, err
}

func (r *slowReader) Read(buffer []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	time.Sleep(r.delay)
	r.done = true
	return copy(buffer, r.content), nil
}

type memoryStore struct {
	objects    map[string][]byte
	promotions []string
	presignErr error
}

func (s *memoryStore) PutStaging(_ context.Context, key string, reader io.Reader, size int64) error {
	content, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	if int64(len(content)) != size {
		return fmt.Errorf("staging size = %d, want %d", len(content), size)
	}
	s.objects[key] = append([]byte(nil), content...)
	return nil
}

func (s *memoryStore) Promote(_ context.Context, stagingKey, objectKey string, expectedSize int64) error {
	content, ok := s.objects[stagingKey]
	if !ok {
		return storage.ErrNotFound
	}
	if int64(len(content)) != expectedSize {
		return storage.ErrObjectConflict
	}
	s.objects[objectKey] = append([]byte(nil), content...)
	delete(s.objects, stagingKey)
	s.promotions = append(s.promotions, stagingKey+"->"+objectKey)
	return nil
}

func (s *memoryStore) Open(_ context.Context, key, _ string) (storage.Object, error) {
	content, ok := s.objects[key]
	if !ok {
		return storage.Object{}, storage.ErrNotFound
	}
	return storage.Object{
		Body: io.NopCloser(bytes.NewReader(content)),
		Info: storage.ObjectInfo{Key: key, Size: int64(len(content))},
	}, nil
}

func (s *memoryStore) Stat(_ context.Context, key string) (storage.ObjectInfo, error) {
	content, ok := s.objects[key]
	if !ok {
		return storage.ObjectInfo{}, storage.ErrNotFound
	}
	return storage.ObjectInfo{Key: key, Size: int64(len(content))}, nil
}

func (*memoryStore) List(context.Context, storage.ListRequest) (storage.ListPage, error) {
	return storage.ListPage{}, nil
}

func (s *memoryStore) Delete(_ context.Context, key string) error {
	delete(s.objects, key)
	return nil
}

func (s *memoryStore) Presign(_ context.Context, key string, _ time.Duration) (string, error) {
	if s.presignErr != nil {
		return "", s.presignErr
	}
	return "https://downloads.example.test/" + key, nil
}

func (*memoryStore) Ready(context.Context) error { return nil }
