package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"superfan.myasustor.com/fanchao/artifact-repository/internal/config"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/database"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/metrics"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/ratelimit"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/storage"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/testenv"
)

func TestRunDispatchesOperationalCommands(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCall string
	}{
		{name: "api", args: []string{"api"}, wantCall: "api"},
		{name: "worker", args: []string{"worker"}, wantCall: "worker"},
		{name: "migrate", args: []string{"migrate"}, wantCall: "migrate"},
		{name: "bootstrap admin", args: []string{"bootstrap-admin", "--name", "operations-admin"}, wantCall: "bootstrap:operations-admin"},
		{name: "keygen", args: []string{"keygen", "--private-key-file", "/keys/private.pem", "--public-key-file", "/keys/public.pem"}, wantCall: "keygen:/keys/private.pem:/keys/public.pem"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := make([]string, 0, 1)
			dependencies := commandDependencies{
				api:     func(context.Context) error { calls = append(calls, "api"); return nil },
				worker:  func(context.Context) error { calls = append(calls, "worker"); return nil },
				migrate: func(context.Context) error { calls = append(calls, "migrate"); return nil },
				bootstrapAdmin: func(_ context.Context, name string) error {
					calls = append(calls, "bootstrap:"+name)
					return nil
				},
				keygen: func(_ context.Context, privatePath, publicPath string) error {
					calls = append(calls, "keygen:"+privatePath+":"+publicPath)
					return nil
				},
			}
			if err := run(t.Context(), tt.args, dependencies); err != nil {
				t.Fatalf("run() error = %v", err)
			}
			if len(calls) != 1 || calls[0] != tt.wantCall {
				t.Fatalf("calls = %v, want [%s]", calls, tt.wantCall)
			}
		})
	}
}

func TestRunRejectsInvalidCommandArguments(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"unknown"},
		{"api", "extra"},
		{"bootstrap-admin"},
		{"bootstrap-admin", "--name", ""},
		{"keygen", "--private-key-file", "/keys/private.pem"},
	} {
		if err := run(t.Context(), args, commandDependencies{}); !errors.Is(err, ErrUsage) {
			t.Fatalf("run(%v) error = %v, want ErrUsage", args, err)
		}
	}
}

func TestDefaultCommandDependenciesAreConfigured(t *testing.T) {
	dependencies := defaultCommandDependencies()
	if dependencies.api == nil || dependencies.worker == nil || dependencies.migrate == nil ||
		dependencies.bootstrapAdmin == nil || dependencies.keygen == nil {
		t.Fatalf("default command dependencies are incomplete: %+v", dependencies)
	}
}

func TestMigrateAndBootstrapCommandsUseMinimalEnvironment(t *testing.T) {
	pool := testenv.StartPostgres(t)
	values := map[string]string{
		"DATABASE_URL": pool.Config().ConnString(),
		"TOKEN_PEPPER": base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x41}, 32)),
	}
	output := new(bytes.Buffer)
	dependencies := newCommandDependencies(commandEnvironment{
		lookup: func(name string) (string, bool) { value, ok := values[name]; return value, ok },
		output: output,
		random: bytes.NewReader(bytes.Repeat([]byte{0x51}, 64)),
	})
	if err := run(t.Context(), []string{"migrate"}, dependencies); err != nil {
		t.Fatalf("run(migrate) error = %v", err)
	}
	if err := run(t.Context(), []string{"bootstrap-admin", "--name", "operations-admin"}, dependencies); err != nil {
		t.Fatalf("run(bootstrap-admin) error = %v", err)
	}
	if bearer := strings.TrimSpace(output.String()); !strings.HasPrefix(bearer, "ar1.") {
		t.Fatalf("bootstrap output = %q, want one bearer token", output.String())
	}
}

func TestKeygenCommandWritesRequestedFilesAndIdentity(t *testing.T) {
	directory := t.TempDir()
	privatePath := filepath.Join(directory, "private.pem")
	publicPath := filepath.Join(directory, "public.pem")
	output := new(bytes.Buffer)
	dependencies := newCommandDependencies(commandEnvironment{
		output: output,
		random: bytes.NewReader(bytes.Repeat([]byte{0x61}, 32)),
	})
	if err := run(t.Context(), []string{
		"keygen", "--private-key-file", privatePath, "--public-key-file", publicPath,
	}, dependencies); err != nil {
		t.Fatalf("run(keygen) error = %v", err)
	}
	privateInfo, err := os.Stat(privatePath)
	if err != nil {
		t.Fatalf("stat private key: %v", err)
	}
	if privateInfo.Mode().Perm() != 0o600 {
		t.Fatalf("private key mode = %o, want 600", privateInfo.Mode().Perm())
	}
	if !strings.Contains(output.String(), "keyId=ed25519:") || !strings.Contains(output.String(), "fingerprint=") {
		t.Fatalf("keygen output = %q", output.String())
	}
}

func TestOpenCheckedDatabaseRejectsSchemaMismatch(t *testing.T) {
	pool := testenv.StartPostgres(t)
	databaseURL := pool.Config().ConnString()

	opened, err := openCheckedDatabase(t.Context(), databaseURL)
	if opened != nil || !errors.Is(err, database.ErrSchemaVersionMismatch) {
		t.Fatalf("openCheckedDatabase(unmigrated) = pool %v error %v", opened, err)
	}
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	opened, err = openCheckedDatabase(t.Context(), databaseURL)
	if err != nil {
		t.Fatalf("openCheckedDatabase(migrated) error = %v", err)
	}
	opened.Close()
}

func TestNewRateLimiterUsesConfiguredBurstAndUploadConcurrency(t *testing.T) {
	limiter, err := newRateLimiter(config.Config{
		RateLimitReadRPS: 1, RateLimitReadBurst: 1,
		RateLimitMutationRPS: 1, RateLimitMutationBurst: 1,
		RateLimitUploadRPS: 100, RateLimitUploadBurst: 10,
		RateLimitUploadConcurrency: 1, RateLimitIdleTTL: 15 * time.Minute,
	})
	if err != nil {
		t.Fatalf("newRateLimiter() error = %v", err)
	}
	tokenID := uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	if decision := limiter.Acquire(tokenID, ratelimit.ClassRead); !decision.Allowed {
		t.Fatalf("first read decision = %+v", decision)
	}
	if decision := limiter.Acquire(tokenID, ratelimit.ClassRead); decision.Allowed {
		t.Fatalf("second read decision = %+v, want burst denial", decision)
	}
	upload := limiter.Acquire(tokenID, ratelimit.ClassUpload)
	if !upload.Allowed || upload.Release == nil {
		t.Fatalf("first upload decision = %+v", upload)
	}
	if decision := limiter.Acquire(tokenID, ratelimit.ClassUpload); decision.Allowed {
		t.Fatalf("second upload decision = %+v, want concurrency denial", decision)
	}
	upload.Release()
}

func TestNewStorageSelectsFilesystemBackend(t *testing.T) {
	store, err := newStorage(config.Config{
		FilesystemRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("newStorage() error = %v", err)
	}
	if _, ok := store.(*storage.Filesystem); !ok {
		t.Fatalf("newStorage() type = %T, want *storage.Filesystem", store)
	}
}

func TestRefreshWorkerMetricsReportsBacklogAndStagingAge(t *testing.T) {
	pool := testenv.StartPostgres(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	accountID := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	tokenID := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	repositoryID := uuid.MustParse("33333333-3333-4333-8333-333333333333")
	sessionID := uuid.MustParse("44444444-4444-4444-8444-444444444444")
	if _, err := pool.Exec(t.Context(), "INSERT INTO service_accounts (id, name) VALUES ($1, 'metrics-worker')", accountID); err != nil {
		t.Fatalf("insert service account: %v", err)
	}
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO api_tokens (id, service_account_id, secret_hmac, scopes, repository_ids, expires_at)
		 VALUES ($1, $2, $3, ARRAY['admin'], '{}', $4)`,
		tokenID, accountID, bytes.Repeat([]byte{0x71}, 32), now.Add(24*time.Hour),
	); err != nil {
		t.Fatalf("insert token: %v", err)
	}
	if _, err := pool.Exec(t.Context(),
		"INSERT INTO repositories (id, key, display_name, created_by) VALUES ($1, 'metrics', 'Metrics', $2)",
		repositoryID, tokenID,
	); err != nil {
		t.Fatalf("insert repository: %v", err)
	}
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO upload_sessions
		 (id, repository_id, logical_path, staging_key, state, lease_owner, lease_expires_at,
		  hard_deadline, last_heartbeat_at, created_by, created_at)
		 VALUES ($1, $2, 'old', $3, 'active', $1, $4, $4, $5, $6, $7)`,
		sessionID, repositoryID, "staging/uploads/"+sessionID.String(), now.Add(time.Hour), now, tokenID, now.Add(-2*time.Hour),
	); err != nil {
		t.Fatalf("insert upload session: %v", err)
	}
	if _, err := pool.Exec(t.Context(),
		"INSERT INTO jobs (kind, state, payload, max_attempts, available_at) VALUES ('cleanup_blob', 'pending', '{}', 10, $1)",
		now,
	); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	registry := metrics.NewRegistry()
	if err := refreshWorkerMetrics(t.Context(), pool, registry, now); err != nil {
		t.Fatalf("refreshWorkerMetrics() error = %v", err)
	}
	response := httptest.NewRecorder()
	registry.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := response.Body.String()
	for _, expected := range []string{
		`artifact_repository_job_backlog{kind="cleanup_blob"} 1`,
		`artifact_repository_job_backlog{kind="cleanup_upload"} 0`,
		`artifact_repository_staging_oldest_age_seconds 7200`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("metrics body missing %q:\n%s", expected, body)
		}
	}
}
