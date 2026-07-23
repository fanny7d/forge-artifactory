package config

import (
	"encoding/base64"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

const keySize = 32

type LookupFunc func(string) (string, bool)

type Config struct {
	DatabaseURL                string
	FilesystemRoot             string
	TokenPepper                []byte
	IdempotencyResponseKey     []byte
	SigningPrivateKeyFile      string
	SigningPublicKeyFile       string
	HTTPAddr                   string
	MaxUploadBytes             int64
	UploadIdleTimeout          time.Duration
	UploadMaxDuration          time.Duration
	UploadLease                time.Duration
	UploadHeartbeat            time.Duration
	PublishLease               time.Duration
	PublishHeartbeat           time.Duration
	PresignTTL                 time.Duration
	OrphanRetention            time.Duration
	IdempotencyTTL             time.Duration
	ReadinessTimeout           time.Duration
	RateLimitReadRPS           float64
	RateLimitReadBurst         int
	RateLimitMutationRPS       float64
	RateLimitMutationBurst     int
	RateLimitUploadRPS         float64
	RateLimitUploadBurst       int
	RateLimitUploadConcurrency int
	RateLimitIdleTTL           time.Duration
}

func FromEnv() (Config, error) {
	return Load(os.LookupEnv)
}

func Load(lookup LookupFunc) (Config, error) {
	if lookup == nil {
		lookup = func(string) (string, bool) { return "", false }
	}

	var cfg Config
	var err error

	if cfg.DatabaseURL, err = required(lookup, "DATABASE_URL"); err != nil {
		return Config{}, err
	}
	if cfg.FilesystemRoot, err = required(lookup, "FILESYSTEM_ROOT"); err != nil {
		return Config{}, err
	}
	if cfg.SigningPrivateKeyFile, err = required(lookup, "SIGNING_PRIVATE_KEY_FILE"); err != nil {
		return Config{}, err
	}
	if cfg.SigningPublicKeyFile, err = required(lookup, "SIGNING_PUBLIC_KEY_FILE"); err != nil {
		return Config{}, err
	}
	if cfg.TokenPepper, err = requiredKey(lookup, "TOKEN_PEPPER"); err != nil {
		return Config{}, err
	}
	if cfg.IdempotencyResponseKey, err = requiredKey(lookup, "IDEMPOTENCY_RESPONSE_KEY"); err != nil {
		return Config{}, err
	}
	cfg.HTTPAddr = stringValue(lookup, "HTTP_ADDR", ":8080")
	if cfg.MaxUploadBytes, err = int64Value(lookup, "MAX_UPLOAD_BYTES", 10<<30); err != nil {
		return Config{}, err
	}
	if cfg.UploadIdleTimeout, err = durationValue(lookup, "UPLOAD_IDLE_TIMEOUT", 10*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.UploadMaxDuration, err = durationValue(lookup, "UPLOAD_MAX_DURATION", 12*time.Hour); err != nil {
		return Config{}, err
	}
	if cfg.UploadLease, err = durationValue(lookup, "UPLOAD_LEASE", 2*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.UploadHeartbeat, err = durationValue(lookup, "UPLOAD_HEARTBEAT", 30*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.PublishLease, err = durationValue(lookup, "PUBLISH_LEASE", 5*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.PublishHeartbeat, err = durationValue(lookup, "PUBLISH_HEARTBEAT", time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.PresignTTL, err = durationValue(lookup, "PRESIGN_TTL", 15*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.OrphanRetention, err = durationValue(lookup, "ORPHAN_RETENTION", 24*time.Hour); err != nil {
		return Config{}, err
	}
	if cfg.IdempotencyTTL, err = durationValue(lookup, "IDEMPOTENCY_TTL", 24*time.Hour); err != nil {
		return Config{}, err
	}
	if cfg.ReadinessTimeout, err = durationValue(lookup, "READINESS_TIMEOUT", 2*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.RateLimitReadRPS, err = positiveFloat64Value(lookup, "RATE_LIMIT_READ_RPS", 50); err != nil {
		return Config{}, err
	}
	if cfg.RateLimitReadBurst, err = positiveIntValue(lookup, "RATE_LIMIT_READ_BURST", 100); err != nil {
		return Config{}, err
	}
	if cfg.RateLimitMutationRPS, err = positiveFloat64Value(lookup, "RATE_LIMIT_MUTATION_RPS", 10); err != nil {
		return Config{}, err
	}
	if cfg.RateLimitMutationBurst, err = positiveIntValue(lookup, "RATE_LIMIT_MUTATION_BURST", 20); err != nil {
		return Config{}, err
	}
	if cfg.RateLimitUploadRPS, err = positiveFloat64Value(lookup, "RATE_LIMIT_UPLOAD_RPS", 2); err != nil {
		return Config{}, err
	}
	if cfg.RateLimitUploadBurst, err = positiveIntValue(lookup, "RATE_LIMIT_UPLOAD_BURST", 4); err != nil {
		return Config{}, err
	}
	if cfg.RateLimitUploadConcurrency, err = positiveIntValue(lookup, "RATE_LIMIT_UPLOAD_CONCURRENCY", 4); err != nil {
		return Config{}, err
	}
	if cfg.RateLimitIdleTTL, err = durationValue(lookup, "RATE_LIMIT_IDLE_TTL", 15*time.Minute); err != nil {
		return Config{}, err
	}

	if cfg.MaxUploadBytes <= 0 {
		return Config{}, fmt.Errorf("MAX_UPLOAD_BYTES must be positive")
	}
	if cfg.UploadHeartbeat >= cfg.UploadLease {
		return Config{}, fmt.Errorf("UPLOAD_HEARTBEAT must be shorter than UPLOAD_LEASE")
	}
	if cfg.PublishHeartbeat >= cfg.PublishLease {
		return Config{}, fmt.Errorf("PUBLISH_HEARTBEAT must be shorter than PUBLISH_LEASE")
	}
	if cfg.UploadIdleTimeout >= cfg.UploadMaxDuration {
		return Config{}, fmt.Errorf("UPLOAD_IDLE_TIMEOUT must be shorter than UPLOAD_MAX_DURATION")
	}
	if cfg.UploadMaxDuration >= cfg.OrphanRetention {
		return Config{}, fmt.Errorf("UPLOAD_MAX_DURATION must be shorter than ORPHAN_RETENTION")
	}

	return cfg, nil
}

func required(lookup LookupFunc, key string) (string, error) {
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s is required", key)
	}
	return value, nil
}

func requiredKey(lookup LookupFunc, name string) ([]byte, error) {
	value, err := required(lookup, name)
	if err != nil {
		return nil, err
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("%s must be base64url without padding: %w", name, err)
	}
	if len(decoded) != keySize {
		return nil, fmt.Errorf("%s must decode to %d bytes", name, keySize)
	}
	return decoded, nil
}

func stringValue(lookup LookupFunc, key, fallback string) string {
	if value, ok := lookup(key); ok && strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}

func boolValue(lookup LookupFunc, key string, fallback bool) (bool, error) {
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", key, err)
	}
	return parsed, nil
}

func int64Value(lookup LookupFunc, key string, fallback int64) (int64, error) {
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", key, err)
	}
	return parsed, nil
}

func positiveIntValue(lookup LookupFunc, key string, fallback int) (int, error) {
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", key, err)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("%s must be positive", key)
	}
	return parsed, nil
}

func positiveFloat64Value(lookup LookupFunc, key string, fallback float64) (float64, error) {
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a number: %w", key, err)
	}
	if parsed <= 0 || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, fmt.Errorf("%s must be a finite positive number", key)
	}
	return parsed, nil
}

func durationValue(lookup LookupFunc, key string, fallback time.Duration) (time.Duration, error) {
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration: %w", key, err)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("%s must be positive", key)
	}
	return parsed, nil
}
