package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func TestMiddlewareAuthenticatesBearerAndAddsActorToContext(t *testing.T) {
	wantActor := Actor{
		TokenID:          uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"),
		ServiceAccountID: uuid.MustParse("bbbbbbbb-cccc-4ddd-8eee-ffffffffffff"),
		Scopes:           NewScopeSet(ScopeAdmin),
	}
	authenticator := &fakeAuthenticator{actor: wantActor}
	called := false
	handler := Middleware(authenticator, func(http.ResponseWriter, *http.Request, error) {
		t.Fatal("authentication error handler was called")
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		actor, ok := ActorFromContext(r.Context())
		if !ok || actor.TokenID != wantActor.TokenID {
			t.Fatalf("ActorFromContext() = %+v, %v", actor, ok)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	request := httptest.NewRequest(http.MethodGet, "/api/v1/repositories", nil)
	request.Header.Set("Authorization", "Bearer ar1.test")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if !called || response.Code != http.StatusNoContent {
		t.Fatalf("called = %v, status = %d", called, response.Code)
	}
	if authenticator.bearer != "ar1.test" {
		t.Fatalf("Authenticate() bearer = %q", authenticator.bearer)
	}
}

func TestMiddlewareRejectsMissingOrInvalidCredentials(t *testing.T) {
	wantErr := errors.New("database unavailable")
	tests := []struct {
		name          string
		authorization string
		authError     error
	}{
		{name: "missing header"},
		{name: "wrong scheme", authorization: "Basic abc"},
		{name: "authentication failure", authorization: "Bearer ar1.bad", authError: wantErr},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			authenticator := &fakeAuthenticator{err: tt.authError}
			handled := false
			handler := Middleware(authenticator, func(w http.ResponseWriter, _ *http.Request, err error) {
				handled = true
				if err == nil {
					t.Fatal("authentication error is nil")
				}
				w.WriteHeader(http.StatusUnauthorized)
			})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("protected handler was called")
			}))

			request := httptest.NewRequest(http.MethodGet, "/api/v1/repositories", nil)
			if tt.authorization != "" {
				request.Header.Set("Authorization", tt.authorization)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if !handled || response.Code != http.StatusUnauthorized {
				t.Fatalf("handled = %v, status = %d", handled, response.Code)
			}
		})
	}
}

type fakeAuthenticator struct {
	actor  Actor
	err    error
	bearer string
}

func (f *fakeAuthenticator) Authenticate(_ context.Context, bearer string) (Actor, error) {
	f.bearer = bearer
	return f.actor, f.err
}
