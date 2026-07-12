package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"regexp"

	"github.com/go-chi/chi/v5"

	identity "superfan.myasustor.com/fanchao/artifact-repository/internal/auth"
	repositorydomain "superfan.myasustor.com/fanchao/artifact-repository/internal/repository"
)

var validName = regexp.MustCompile(`^[a-z][a-z0-9._-]{1,63}$`)

type RepositoryService interface {
	Create(context.Context, repositorydomain.CreateRequest) (repositorydomain.Repository, error)
	Get(context.Context, identity.Actor, string) (repositorydomain.Repository, error)
	List(context.Context, identity.Actor, repositorydomain.ListRequest) (repositorydomain.Page, error)
}

type repositoryHandlers struct {
	service RepositoryService
}

func registerRepositoryRoutes(router chi.Router, service RepositoryService) {
	if service == nil {
		return
	}
	handlers := repositoryHandlers{service: service}
	router.Post("/repositories", handlers.create)
	router.Get("/repositories", handlers.list)
	router.Get("/repositories/{repo}", handlers.get)
}

func (h repositoryHandlers) create(w http.ResponseWriter, r *http.Request) {
	var body CreateRepositoryRequest
	raw, err := decodeJSONRequest(w, r, &body)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	if !validName.MatchString(body.Key) {
		writeHandlerError(w, r, repositorydomain.ErrInvalidRequest)
		return
	}
	actor, ok := identity.ActorFromContext(r.Context())
	if !ok {
		writeAuthenticationError(w, r, identity.ErrInvalidToken)
		return
	}
	mutation, err := mutationFromRequest(r, actor, "/api/v1/repositories", raw)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	created, err := h.service.Create(r.Context(), repositorydomain.CreateRequest{
		Mutation:    mutation,
		Key:         body.Key,
		DisplayName: body.DisplayName,
	})
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, repositoryDTO(created))
}

func (h repositoryHandlers) list(w http.ResponseWriter, r *http.Request) {
	actor, ok := identity.ActorFromContext(r.Context())
	if !ok {
		writeAuthenticationError(w, r, identity.ErrInvalidToken)
		return
	}
	limit, err := listLimit(r)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	request := repositorydomain.ListRequest{Limit: limit}
	if encoded := r.URL.Query().Get("cursor"); encoded != "" {
		cursor, err := decodeKeyCursor(encoded, "repositories")
		if err != nil {
			writeHandlerError(w, r, err)
			return
		}
		request.After = &repositorydomain.Cursor{Key: cursor.Key}
	}
	page, err := h.service.List(r.Context(), actor, request)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	items := make([]Repository, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, repositoryDTO(item))
	}
	writeJSON(w, http.StatusOK, RepositoryPage{Items: items, NextCursor: encodeRepositoryCursor(page.Next)})
}

func (h repositoryHandlers) get(w http.ResponseWriter, r *http.Request) {
	actor, ok := identity.ActorFromContext(r.Context())
	if !ok {
		writeAuthenticationError(w, r, identity.ErrInvalidToken)
		return
	}
	repositoryKey := chi.URLParam(r, "repo")
	if !validName.MatchString(repositoryKey) {
		writeHandlerError(w, r, repositorydomain.ErrInvalidRequest)
		return
	}
	repository, err := h.service.Get(r.Context(), actor, repositoryKey)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, repositoryDTO(repository))
}

func repositoryDTO(repository repositorydomain.Repository) Repository {
	return Repository{
		Id:          repository.ID,
		Key:         repository.Key,
		DisplayName: repository.DisplayName,
		Type:        repository.Type,
		CreatedAt:   repository.CreatedAt,
	}
}

func decodeKeyCursor(encoded, kind string) (cursorEnvelope, error) {
	if len(encoded) > 512 {
		return cursorEnvelope{}, identity.ErrInvalidRequest
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return cursorEnvelope{}, identity.ErrInvalidRequest
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var cursor cursorEnvelope
	if err := decoder.Decode(&cursor); err != nil || cursor.Kind != kind || !validName.MatchString(cursor.Key) {
		return cursorEnvelope{}, identity.ErrInvalidRequest
	}
	return cursor, nil
}

func encodeRepositoryCursor(cursor *repositorydomain.Cursor) *PageCursor {
	if cursor == nil {
		return nil
	}
	return encodeCursor(cursorEnvelope{Kind: "repositories", Key: cursor.Key})
}
