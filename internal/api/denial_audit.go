package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"superfan.myasustor.com/fanchao/artifact-repository/internal/audit"
	identity "superfan.myasustor.com/fanchao/artifact-repository/internal/auth"
)

type denialAuditContextKey struct{}

func denialAuditMiddleware(service AuditService) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), denialAuditContextKey{}, service)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func recordDeniedRequest(r *http.Request, code string) error {
	service, _ := r.Context().Value(denialAuditContextKey{}).(AuditService)
	if service == nil {
		return nil
	}
	actor, _ := identity.ActorFromContext(r.Context())
	route := chi.RouteContext(r.Context()).RoutePattern()
	if route == "" || len(route) > 256 {
		route = "/api/v1"
	}
	method := r.Method
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete:
	default:
		method = "OTHER"
	}
	_, err := service.RecordStandalone(r.Context(), audit.Event{
		ActorTokenID: actor.TokenID,
		Action:       "http.request",
		ResourceType: "api_route",
		ResourceID:   route,
		Outcome:      audit.OutcomeDenied,
		Code:         code,
		RequestID:    requestIDFromContext(r.Context()),
		Details:      map[string]any{"method": method},
	})
	if err != nil {
		return fmt.Errorf("record denied request: %w", err)
	}
	return nil
}

func writeAuditUnavailable(w http.ResponseWriter, r *http.Request) {
	writeRequestProblem(w, r, Problem{
		Type:   "about:blank",
		Title:  "Service Unavailable",
		Status: http.StatusServiceUnavailable,
		Code:   "audit-unavailable",
	})
}
