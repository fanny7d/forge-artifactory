package auth

import (
	"context"
	"net/http"
	"strings"
)

type Authenticator interface {
	Authenticate(context.Context, string) (Actor, error)
}

type AuthenticationErrorHandler func(http.ResponseWriter, *http.Request, error)

type actorContextKey struct{}

func Middleware(authenticator Authenticator, onError AuthenticationErrorHandler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			bearer, err := bearerFromHeader(r.Header.Get("Authorization"))
			if err != nil {
				onError(w, r, err)
				return
			}
			actor, err := authenticator.Authenticate(r.Context(), bearer)
			if err != nil {
				onError(w, r, err)
				return
			}
			ctx := context.WithValue(r.Context(), actorContextKey{}, actor)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func ActorFromContext(ctx context.Context) (Actor, bool) {
	actor, ok := ctx.Value(actorContextKey{}).(Actor)
	return actor, ok
}

func bearerFromHeader(value string) (string, error) {
	parts := strings.Fields(value)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", ErrInvalidToken
	}
	return parts[1], nil
}
