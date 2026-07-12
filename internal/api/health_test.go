package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type readinessProbe struct {
	calls int
	err   error
}

func (p *readinessProbe) Ready(context.Context) error {
	p.calls++
	return p.err
}

func TestHealthzDoesNotProbeDependencies(t *testing.T) {
	probe := &readinessProbe{err: errors.New("unavailable")}
	handler := NewServer(Dependencies{Readiness: probe})

	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if probe.calls != 0 {
		t.Fatalf("readiness calls = %d, want 0", probe.calls)
	}
}

func TestReadyzReflectsDependencyState(t *testing.T) {
	tests := []struct {
		name       string
		probeError error
		wantStatus int
	}{
		{name: "ready", wantStatus: http.StatusOK},
		{name: "dependency unavailable", probeError: errors.New("postgres unavailable"), wantStatus: http.StatusServiceUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			probe := &readinessProbe{err: tt.probeError}
			handler := NewServer(Dependencies{Readiness: probe})

			request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			if response.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, tt.wantStatus)
			}
			if probe.calls != 1 {
				t.Fatalf("readiness calls = %d, want 1", probe.calls)
			}
		})
	}
}

func TestUnknownRouteUsesProblemDetails(t *testing.T) {
	handler := NewServer(Dependencies{Readiness: &readinessProbe{}})
	request := httptest.NewRequest(http.MethodGet, "/missing", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
	if got := response.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", got)
	}
}

func TestMetricsEndpointUsesConfiguredHandler(t *testing.T) {
	calls := 0
	handler := NewServer(Dependencies{
		Readiness: &readinessProbe{},
		Metrics: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			w.Header().Set("Content-Type", "text/plain; version=0.0.4")
			_, _ = w.Write([]byte("artifact_repository_uploads_total 1\n"))
		}),
	})
	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || calls != 1 {
		t.Fatalf("metrics response = status %d calls %d, want 200/1", response.Code, calls)
	}
	if response.Body.String() != "artifact_repository_uploads_total 1\n" {
		t.Fatalf("metrics body = %q", response.Body.String())
	}
}
