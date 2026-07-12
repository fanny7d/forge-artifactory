package storage

import (
	"errors"
	"net/url"
	"testing"
	"time"
)

func TestMinIOPresignUsesPublicEndpoint(t *testing.T) {
	store, err := NewMinIO(MinIOOptions{
		Endpoint:       "minio.internal:9000",
		PublicEndpoint: "downloads.example.test",
		AccessKey:      "test-access",
		SecretKey:      "test-secret",
		Bucket:         "artifacts",
		Region:         "us-east-1",
		UseTLS:         true,
	})
	if err != nil {
		t.Fatalf("NewMinIO() error = %v", err)
	}

	signed, err := store.Presign(t.Context(), "blobs/sha256/aa/bb/value", 15*time.Minute)
	if err != nil {
		t.Fatalf("Presign() error = %v", err)
	}
	parsed, err := url.Parse(signed)
	if err != nil {
		t.Fatalf("parse signed URL: %v", err)
	}
	if parsed.Host != "downloads.example.test" || parsed.Scheme != "https" {
		t.Fatalf("signed URL = %s", signed)
	}
	if parsed.Host == "minio.internal:9000" {
		t.Fatal("signed URL exposed the internal endpoint")
	}
}

func TestMinIOPresignRequiresPublicEndpoint(t *testing.T) {
	store, err := NewMinIO(MinIOOptions{
		Endpoint:  "minio.internal:9000",
		AccessKey: "test-access",
		SecretKey: "test-secret",
		Bucket:    "artifacts",
		Region:    "us-east-1",
	})
	if err != nil {
		t.Fatalf("NewMinIO() error = %v", err)
	}
	if _, err := store.Presign(t.Context(), "blob", time.Minute); !errors.Is(err, ErrPublicEndpointUnavailable) {
		t.Fatalf("Presign() error = %v, want ErrPublicEndpointUnavailable", err)
	}
}
