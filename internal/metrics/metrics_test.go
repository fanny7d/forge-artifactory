package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMetricsExposeBoundedLabels(t *testing.T) {
	registry := NewRegistry()
	registry.ObserveUpload("success", 1024, 250*time.Millisecond)
	registry.ObserveUpload("repositories/private/token-ar1.secret", 2048, time.Second)
	registry.ObserveDownload("not_found")
	registry.ObserveResolve("success")
	registry.ObserveBlobDedup(true)
	registry.ObservePublish("failed")
	registry.ObservePromotion("conflict")
	registry.ObserveSigningFailure("invalid_key")
	registry.SetStagingOldestAge(90 * time.Second)
	registry.SetJobBacklog("cleanup_blob", 3)
	registry.SetJobBacklog("tenant-specific-job", 4)
	registry.ObserveDependency("postgres", "success", 20*time.Millisecond)
	registry.ObserveDependency("https://secret.example/path", "failed", time.Second)

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	registry.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", response.Code)
	}
	body := response.Body.String()
	for _, expected := range []string{
		`artifact_repository_uploads_total{result="success"} 1`,
		`artifact_repository_uploads_total{result="unknown"} 1`,
		`artifact_repository_job_backlog{kind="cleanup_blob"} 3`,
		`artifact_repository_job_backlog{kind="unknown"} 4`,
		`artifact_repository_dependency_requests_total{dependency="postgres",result="success"} 1`,
		`artifact_repository_dependency_requests_total{dependency="unknown",result="failed"} 1`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("metrics body missing %q:\n%s", expected, body)
		}
	}
	for _, secret := range []string{"repositories/private", "ar1.secret", "secret.example", "tenant-specific-job"} {
		if strings.Contains(body, secret) {
			t.Fatalf("metrics body contains unbounded value %q", secret)
		}
	}
}

func TestInstrumentHTTPRecordsBoundedOperationResults(t *testing.T) {
	registry := NewRegistry()
	handler := registry.InstrumentHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/resolve"):
			http.Error(w, "missing", http.StatusNotFound)
		case strings.HasSuffix(r.URL.Path, "/publish"):
			http.Error(w, "failed", http.StatusInternalServerError)
		case strings.HasSuffix(r.URL.Path, "/promotions"):
			http.Error(w, "conflict", http.StatusConflict)
		default:
			w.WriteHeader(http.StatusCreated)
		}
	}))
	requests := []*http.Request{
		httptest.NewRequest(http.MethodPut, "/api/v1/repositories/repo/artifacts/app.bin", strings.NewReader("payload")),
		httptest.NewRequest(http.MethodGet, "/api/v1/repositories/repo/artifacts/app.bin", nil),
		httptest.NewRequest(http.MethodGet, "/api/v1/repositories/repo/packages/app/channels/stable/resolve", nil),
		httptest.NewRequest(http.MethodPost, "/api/v1/repositories/repo/packages/app/releases/1.0.0/publish", nil),
		httptest.NewRequest(http.MethodPost, "/api/v1/repositories/repo/packages/app/channels/stable/promotions", nil),
	}
	for _, request := range requests {
		handler.ServeHTTP(httptest.NewRecorder(), request)
	}

	response := httptest.NewRecorder()
	registry.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := response.Body.String()
	for _, expected := range []string{
		`artifact_repository_uploads_total{result="success"} 1`,
		`artifact_repository_downloads_total{result="success"} 1`,
		`artifact_repository_resolves_total{result="not_found"} 1`,
		`artifact_repository_publishes_total{result="failed"} 1`,
		`artifact_repository_promotions_total{result="conflict"} 1`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("metrics body missing %q:\n%s", expected, body)
		}
	}
}
