package api

import (
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	identity "superfan.myasustor.com/fanchao/artifact-repository/internal/auth"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/ratelimit"
)

type RequestLimiter interface {
	Acquire(uuid.UUID, ratelimit.Class) ratelimit.Decision
}

func rateLimitMiddleware(limiter RequestLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			actor, ok := identity.ActorFromContext(r.Context())
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			class, ok := requestLimitClass(r.Method)
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			decision := limiter.Acquire(actor.TokenID, class)
			if !decision.Allowed {
				if err := recordDeniedRequest(r, "rate-limit-exceeded"); err != nil {
					writeAuditUnavailable(w, r)
					return
				}
				w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(decision.RetryAfter)))
				writeRequestProblem(w, r, Problem{
					Type:   "about:blank",
					Title:  "Too Many Requests",
					Status: http.StatusTooManyRequests,
					Code:   "rate-limit-exceeded",
				})
				return
			}
			if decision.Release != nil {
				defer decision.Release()
			}
			next.ServeHTTP(w, r)
		})
	}
}

func requestLimitClass(method string) (ratelimit.Class, bool) {
	switch method {
	case http.MethodGet, http.MethodHead:
		return ratelimit.ClassRead, true
	case http.MethodPost, http.MethodDelete:
		return ratelimit.ClassMutation, true
	case http.MethodPut:
		return ratelimit.ClassUpload, true
	default:
		return "", false
	}
}

func retryAfterSeconds(delay time.Duration) int {
	seconds := int(math.Ceil(delay.Seconds()))
	if seconds < 1 {
		return 1
	}
	return seconds
}
