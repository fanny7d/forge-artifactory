package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequestIDIsReturnedInHeaderAndProblem(t *testing.T) {
	handler := NewServer(Dependencies{Readiness: &readinessProbe{}})
	request := httptest.NewRequest(http.MethodGet, "/missing", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	requestID := response.Header().Get("X-Request-ID")
	if requestID == "" {
		t.Fatal("X-Request-ID is empty")
	}
	var problem map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem["requestId"] != requestID {
		t.Fatalf("problem requestId = %v, want %q", problem["requestId"], requestID)
	}
}

func TestRequestIDAcceptsBoundedCallerValue(t *testing.T) {
	handler := NewServer(Dependencies{Readiness: &readinessProbe{}})
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	request.Header.Set("X-Request-ID", "ci-run_42")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if got := response.Header().Get("X-Request-ID"); got != "ci-run_42" {
		t.Fatalf("X-Request-ID = %q, want ci-run_42", got)
	}
}

func TestRequestIDRejectsUnboundedCallerValue(t *testing.T) {
	handler := NewServer(Dependencies{Readiness: &readinessProbe{}})
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	request.Header.Set("X-Request-ID", string(make([]byte, 65)))
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if got := response.Header().Get("X-Request-ID"); got == request.Header.Get("X-Request-ID") {
		t.Fatal("unbounded caller request ID was accepted")
	}
}
