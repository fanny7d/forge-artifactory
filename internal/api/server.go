package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	identity "superfan.myasustor.com/fanchao/artifact-repository/internal/auth"
)

type Dependencies struct {
	Readiness        ReadinessChecker
	ReadinessTimeout time.Duration
	Metrics          http.Handler
	Authenticator    identity.Authenticator
	RateLimiter      RequestLimiter
	Identity         IdentityService
	Audit            AuditService
	Repositories     RepositoryService
	Artifacts        ArtifactService
	Packages         PackageService
	Drafts           DraftService
	Publisher        ReleasePublisher
	Channels         ChannelService
}

func NewServer(dependencies Dependencies) http.Handler {
	timeout := dependencies.ReadinessTimeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}

	router := chi.NewRouter()
	router.Use(requestIDMiddleware)
	router.Get("/healthz", healthz)
	router.Get("/readyz", readyz(dependencies.Readiness, timeout))
	if dependencies.Metrics != nil {
		router.Method(http.MethodGet, "/metrics", dependencies.Metrics)
	}
	if dependencies.Authenticator != nil {
		router.Route("/api/v1", func(router chi.Router) {
			if dependencies.Audit != nil {
				router.Use(denialAuditMiddleware(dependencies.Audit))
			}
			router.Use(identity.Middleware(dependencies.Authenticator, writeAuthenticationError))
			if dependencies.RateLimiter != nil {
				router.Use(rateLimitMiddleware(dependencies.RateLimiter))
			}
			registerAuthRoutes(router, dependencies.Identity, dependencies.Audit)
			registerRepositoryRoutes(router, dependencies.Repositories)
			registerArtifactRoutes(router, dependencies.Artifacts)
			registerPackageRoutes(router, dependencies.Packages)
			registerDraftRoutes(router, dependencies.Drafts)
			registerPublishRoutes(router, dependencies.Publisher)
			registerChannelRoutes(router, dependencies.Channels)
		})
	}
	router.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeRequestProblem(w, r, Problem{
			Type:   "about:blank",
			Title:  "Not Found",
			Status: http.StatusNotFound,
			Code:   "route-not-found",
		})
	})
	return router
}
