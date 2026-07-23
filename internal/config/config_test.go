package config

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestLoadRejectsMissingRequiredValues(t *testing.T) {
	_, err := Load(mapLookup(nil))
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("Load() error = %v, want missing DATABASE_URL", err)
	}
}

func TestLoadAppliesDocumentedDefaults(t *testing.T) {
	values := validValues()

	cfg, err := Load(mapLookup(values))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.HTTPAddr != ":8080" {
		t.Fatalf("HTTPAddr = %q, want :8080", cfg.HTTPAddr)
	}
	if cfg.FilesystemRoot != "/var/lib/artifact-repository" {
		t.Fatalf("FilesystemRoot = %q", cfg.FilesystemRoot)
	}
	if cfg.MaxUploadBytes != 10<<30 {
		t.Fatalf("MaxUploadBytes = %d, want %d", cfg.MaxUploadBytes, int64(10<<30))
	}
	if cfg.UploadIdleTimeout != 10*time.Minute {
		t.Fatalf("UploadIdleTimeout = %s, want 10m", cfg.UploadIdleTimeout)
	}
	if cfg.UploadMaxDuration != 12*time.Hour {
		t.Fatalf("UploadMaxDuration = %s, want 12h", cfg.UploadMaxDuration)
	}
	if cfg.UploadLease != 2*time.Minute {
		t.Fatalf("UploadLease = %s, want 2m", cfg.UploadLease)
	}
	if cfg.UploadHeartbeat != 30*time.Second {
		t.Fatalf("UploadHeartbeat = %s, want 30s", cfg.UploadHeartbeat)
	}
	if cfg.PublishLease != 5*time.Minute {
		t.Fatalf("PublishLease = %s, want 5m", cfg.PublishLease)
	}
	if cfg.PublishHeartbeat != time.Minute {
		t.Fatalf("PublishHeartbeat = %s, want 1m", cfg.PublishHeartbeat)
	}
	if cfg.PresignTTL != 15*time.Minute {
		t.Fatalf("PresignTTL = %s, want 15m", cfg.PresignTTL)
	}
	if cfg.OrphanRetention != 24*time.Hour {
		t.Fatalf("OrphanRetention = %s, want 24h", cfg.OrphanRetention)
	}
	if cfg.IdempotencyTTL != 24*time.Hour {
		t.Fatalf("IdempotencyTTL = %s, want 24h", cfg.IdempotencyTTL)
	}
	if cfg.RateLimitReadRPS != 50 || cfg.RateLimitReadBurst != 100 ||
		cfg.RateLimitMutationRPS != 10 || cfg.RateLimitMutationBurst != 20 ||
		cfg.RateLimitUploadRPS != 2 || cfg.RateLimitUploadBurst != 4 ||
		cfg.RateLimitUploadConcurrency != 4 || cfg.RateLimitIdleTTL != 15*time.Minute {
		t.Fatalf("rate limit defaults = read %v/%d mutation %v/%d upload %v/%d concurrency %d idle %s",
			cfg.RateLimitReadRPS, cfg.RateLimitReadBurst,
			cfg.RateLimitMutationRPS, cfg.RateLimitMutationBurst,
			cfg.RateLimitUploadRPS, cfg.RateLimitUploadBurst,
			cfg.RateLimitUploadConcurrency, cfg.RateLimitIdleTTL)
	}
}

func TestLoadRejectsInvalidStorageConfiguration(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]string)
		want   string
	}{
		{
			name: "filesystem root required",
			mutate: func(values map[string]string) {
				delete(values, "FILESYSTEM_ROOT")
			},
			want: "FILESYSTEM_ROOT",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := validValues()
			tt.mutate(values)
			_, err := Load(mapLookup(values))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load() error = %v, want error containing %q", err, tt.want)
			}
		})
	}
}

func TestLoadRejectsInvalidKeyLengthsAndTimingRelationships(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]string)
		want   string
	}{
		{
			name: "short token pepper",
			mutate: func(values map[string]string) {
				values["TOKEN_PEPPER"] = base64.RawURLEncoding.EncodeToString(make([]byte, 31))
			},
			want: "TOKEN_PEPPER",
		},
		{
			name: "short response key",
			mutate: func(values map[string]string) {
				values["IDEMPOTENCY_RESPONSE_KEY"] = base64.RawURLEncoding.EncodeToString(make([]byte, 16))
			},
			want: "IDEMPOTENCY_RESPONSE_KEY",
		},
		{
			name: "heartbeat is not shorter than lease",
			mutate: func(values map[string]string) {
				values["UPLOAD_HEARTBEAT"] = "2m"
			},
			want: "UPLOAD_HEARTBEAT",
		},
		{
			name: "read rate is positive",
			mutate: func(values map[string]string) {
				values["RATE_LIMIT_READ_RPS"] = "0"
			},
			want: "RATE_LIMIT_READ_RPS",
		},
		{
			name: "upload concurrency is positive",
			mutate: func(values map[string]string) {
				values["RATE_LIMIT_UPLOAD_CONCURRENCY"] = "0"
			},
			want: "RATE_LIMIT_UPLOAD_CONCURRENCY",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := validValues()
			tt.mutate(values)

			_, err := Load(mapLookup(values))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load() error = %v, want error containing %q", err, tt.want)
			}
		})
	}
}

func validValues() map[string]string {
	key := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	return map[string]string{
		"DATABASE_URL":             "postgres://artifact:secret@localhost/artifact",
		"FILESYSTEM_ROOT":          "/var/lib/artifact-repository",
		"TOKEN_PEPPER":             key,
		"IDEMPOTENCY_RESPONSE_KEY": key,
		"SIGNING_PRIVATE_KEY_FILE": "/keys/private.pem",
		"SIGNING_PUBLIC_KEY_FILE":  "/keys/public.pem",
	}
}

func mapLookup(values map[string]string) LookupFunc {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}
