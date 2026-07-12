package api

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	identity "superfan.myasustor.com/fanchao/artifact-repository/internal/auth"
	releasedomain "superfan.myasustor.com/fanchao/artifact-repository/internal/release"
)

type PackageService interface {
	Create(context.Context, releasedomain.CreatePackageRequest) (releasedomain.Package, error)
	Get(context.Context, identity.Actor, string, string) (releasedomain.Package, error)
	List(context.Context, identity.Actor, string, releasedomain.PackageListRequest) (releasedomain.PackagePage, error)
}

type packageHandlers struct {
	service PackageService
}

func registerPackageRoutes(router chi.Router, service PackageService) {
	if service == nil {
		return
	}
	handlers := packageHandlers{service: service}
	router.Post("/repositories/{repo}/packages", handlers.create)
	router.Get("/repositories/{repo}/packages", handlers.list)
	router.Get("/repositories/{repo}/packages/{package}", handlers.get)
}

func (h packageHandlers) create(w http.ResponseWriter, r *http.Request) {
	actor, ok := identity.ActorFromContext(r.Context())
	if !ok {
		writeAuthenticationError(w, r, identity.ErrInvalidToken)
		return
	}
	repositoryKey := chi.URLParam(r, "repo")
	if !validName.MatchString(repositoryKey) {
		writeHandlerError(w, r, releasedomain.ErrInvalidRequest)
		return
	}
	var body CreatePackageRequest
	raw, err := decodeJSONRequest(w, r, &body)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	if !validName.MatchString(body.Name) {
		writeHandlerError(w, r, releasedomain.ErrInvalidRequest)
		return
	}
	canonicalResource := "/api/v1/repositories/" + repositoryKey + "/packages"
	mutation, err := mutationFromRequest(r, actor, canonicalResource, raw)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	created, err := h.service.Create(r.Context(), releasedomain.CreatePackageRequest{
		Mutation:      mutation,
		RepositoryKey: repositoryKey,
		Name:          body.Name,
	})
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, packageDTO(created))
}

func (h packageHandlers) list(w http.ResponseWriter, r *http.Request) {
	actor, repositoryKey, ok := scopedActorAndRepository(w, r)
	if !ok {
		return
	}
	limit, err := listLimit(r)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	request := releasedomain.PackageListRequest{Limit: limit}
	kind := "packages:" + repositoryKey
	if encoded := r.URL.Query().Get("cursor"); encoded != "" {
		cursor, err := decodeCursor(encoded, kind)
		if err != nil {
			writeHandlerError(w, r, err)
			return
		}
		request.After = &releasedomain.PackageCursor{CreatedAt: cursor.CreatedAt, ID: cursor.ID}
	}
	page, err := h.service.List(r.Context(), actor, repositoryKey, request)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	items := make([]Package, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, packageDTO(item))
	}
	writeJSON(w, http.StatusOK, PackagePage{Items: items, NextCursor: encodePackageCursor(kind, page.Next)})
}

func (h packageHandlers) get(w http.ResponseWriter, r *http.Request) {
	actor, repositoryKey, ok := scopedActorAndRepository(w, r)
	if !ok {
		return
	}
	packageName := chi.URLParam(r, "package")
	if !validName.MatchString(packageName) {
		writeHandlerError(w, r, releasedomain.ErrInvalidRequest)
		return
	}
	item, err := h.service.Get(r.Context(), actor, repositoryKey, packageName)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, packageDTO(item))
}

func scopedActorAndRepository(w http.ResponseWriter, r *http.Request) (identity.Actor, string, bool) {
	actor, ok := identity.ActorFromContext(r.Context())
	if !ok {
		writeAuthenticationError(w, r, identity.ErrInvalidToken)
		return identity.Actor{}, "", false
	}
	repositoryKey := chi.URLParam(r, "repo")
	if !validName.MatchString(repositoryKey) {
		writeHandlerError(w, r, releasedomain.ErrInvalidRequest)
		return identity.Actor{}, "", false
	}
	return actor, repositoryKey, true
}

func packageDTO(item releasedomain.Package) Package {
	channels := make([]PackageChannels, len(item.Channels))
	for index, channel := range item.Channels {
		channels[index] = PackageChannels(channel)
	}
	return Package{
		Id:         item.ID,
		Repository: item.RepositoryKey,
		Name:       item.Name,
		Channels:   channels,
		CreatedAt:  item.CreatedAt,
	}
}

func encodePackageCursor(kind string, cursor *releasedomain.PackageCursor) *PageCursor {
	if cursor == nil {
		return nil
	}
	return encodeCursor(cursorEnvelope{Kind: kind, CreatedAt: cursor.CreatedAt, ID: cursor.ID})
}
