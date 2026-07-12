package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"superfan.myasustor.com/fanchao/artifact-repository/internal/ratelimit"
)

func TestRateLimitMiddlewareClassifiesRequestsAndReturnsProblem(t *testing.T) {
	actor := adminAPIActor()
	tests := []struct {
		name      string
		method    string
		path      string
		body      []byte
		headers   map[string]string
		configure func(*Dependencies)
		wantClass ratelimit.Class
	}{
		{
			name: "read", method: http.MethodGet, path: "/api/v1/repositories",
			configure: func(dependencies *Dependencies) {
				dependencies.Repositories = &repositoryServiceStub{}
			},
			wantClass: ratelimit.ClassRead,
		},
		{
			name: "mutation", method: http.MethodPost, path: "/api/v1/repositories/repo-a/packages",
			body:    []byte(`{"name":"edgecli"}`),
			headers: map[string]string{"Content-Type": "application/json", "Idempotency-Key": "create-package"},
			configure: func(dependencies *Dependencies) {
				dependencies.Packages = &packageServiceStub{}
			},
			wantClass: ratelimit.ClassMutation,
		},
		{
			name: "upload", method: http.MethodPut, path: "/api/v1/repositories/repo-a/artifacts/linux/arm64/app",
			body: []byte("payload"), headers: map[string]string{"Content-Type": "application/octet-stream"},
			configure: func(dependencies *Dependencies) {
				dependencies.Artifacts = &artifactServiceStub{}
			},
			wantClass: ratelimit.ClassUpload,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			limiter := &requestLimiterStub{decisions: []ratelimit.Decision{{RetryAfter: 1500 * time.Millisecond}}}
			identityService := &identityServiceStub{actor: actor}
			auditService := &auditServiceStub{}
			dependencies := Dependencies{
				Readiness: &readinessProbe{}, Authenticator: identityService, RateLimiter: limiter, Audit: auditService,
			}
			tt.configure(&dependencies)
			handler := NewServer(dependencies)
			request := httptest.NewRequest(tt.method, tt.path, bytes.NewReader(tt.body))
			request.Header.Set("Authorization", "Bearer ar1.valid")
			for name, value := range tt.headers {
				request.Header.Set(name, value)
			}
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429; body = %s", response.Code, response.Body.String())
			}
			if response.Header().Get("Retry-After") != "2" {
				t.Fatalf("Retry-After = %q, want 2", response.Header().Get("Retry-After"))
			}
			var problem Problem
			decodeJSONBodyForTest(t, response.Body.Bytes(), &problem)
			if problem.Code != "rate-limit-exceeded" || problem.RequestId == "" {
				t.Fatalf("problem = %+v", problem)
			}
			if len(limiter.tokens) != 1 || limiter.tokens[0] != actor.TokenID || limiter.classes[0] != tt.wantClass {
				t.Fatalf("limiter calls = tokens %v classes %v", limiter.tokens, limiter.classes)
			}
			if len(auditService.recorded) != 1 {
				t.Fatalf("rate limit audit events = %d, want 1", len(auditService.recorded))
			}
			assertDeniedAuditEvent(t, auditService.recorded[0], actor.TokenID, "rate-limit-exceeded")
		})
	}
}

func TestRateLimitDenialFailsClosedWhenAuditIsUnavailable(t *testing.T) {
	limiter := &requestLimiterStub{decisions: []ratelimit.Decision{{RetryAfter: time.Second}}}
	identityService := &identityServiceStub{actor: adminAPIActor()}
	handler := NewServer(Dependencies{
		Readiness:     &readinessProbe{},
		Authenticator: identityService,
		RateLimiter:   limiter,
		Repositories:  &repositoryServiceStub{},
		Audit:         &auditServiceStub{recordError: errors.New("database unavailable")},
	})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/repositories", nil)
	request.Header.Set("Authorization", "Bearer ar1.valid")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Retry-After") != "" {
		t.Fatalf("Retry-After = %q, want empty for audit failure", response.Header().Get("Retry-After"))
	}
	var problem Problem
	decodeJSONBodyForTest(t, response.Body.Bytes(), &problem)
	if problem.Code != "audit-unavailable" {
		t.Fatalf("problem = %+v", problem)
	}
}

func TestRateLimitMiddlewareReleasesUploadPermitAfterHandler(t *testing.T) {
	released := 0
	limiter := &requestLimiterStub{decisions: []ratelimit.Decision{{
		Allowed: true,
		Release: func() { released++ },
	}}}
	identityService := &identityServiceStub{actor: adminAPIActor()}
	handler := NewServer(Dependencies{
		Readiness: &readinessProbe{}, Authenticator: identityService, RateLimiter: limiter,
		Artifacts: &artifactServiceStub{},
	})
	request := httptest.NewRequest(
		http.MethodPut,
		"/api/v1/repositories/repo-a/artifacts/linux/arm64/app",
		bytes.NewBufferString("payload"),
	)
	request.Header.Set("Authorization", "Bearer ar1.valid")
	request.Header.Set("Content-Type", "application/octet-stream")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if released != 1 {
		t.Fatalf("upload permit releases = %d, want 1", released)
	}
}

type requestLimiterStub struct {
	decisions []ratelimit.Decision
	tokens    []uuid.UUID
	classes   []ratelimit.Class
}

func (s *requestLimiterStub) Acquire(tokenID uuid.UUID, class ratelimit.Class) ratelimit.Decision {
	s.tokens = append(s.tokens, tokenID)
	s.classes = append(s.classes, class)
	if len(s.decisions) == 0 {
		return ratelimit.Decision{Allowed: true}
	}
	decision := s.decisions[0]
	s.decisions = s.decisions[1:]
	return decision
}

func decodeJSONBodyForTest(t *testing.T, body []byte, destination any) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(destination); err != nil {
		t.Fatalf("decode JSON response: %v", err)
	}
}
