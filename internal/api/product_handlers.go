package api

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	identity "superfan.myasustor.com/fanchao/artifact-repository/internal/auth"
	productdomain "superfan.myasustor.com/fanchao/artifact-repository/internal/product"
)

type ProductService interface {
	Create(context.Context, productdomain.CreateRequest) (productdomain.Product, error)
	Get(context.Context, identity.Actor, string) (productdomain.Product, error)
	List(context.Context, identity.Actor, productdomain.ListRequest) (productdomain.Page, error)
	RotateInstallKey(context.Context, productdomain.RotateInstallKeyRequest) (productdomain.Product, error)
	GetByInstallKey(context.Context, uuid.UUID) (productdomain.Product, error)
}

type productHandlers struct {
	service ProductService
}

func registerProductRoutes(router chi.Router, service ProductService) {
	if service == nil {
		return
	}
	handlers := productHandlers{service: service}
	router.Post("/products", handlers.create)
	router.Get("/products", handlers.list)
	router.Get("/products/{product}", handlers.get)
	router.Post("/products/{product}/install-key/rotate", handlers.rotateInstallKey)
}

func (h productHandlers) create(w http.ResponseWriter, r *http.Request) {
	actor, ok := identity.ActorFromContext(r.Context())
	if !ok {
		writeAuthenticationError(w, r, identity.ErrInvalidToken)
		return
	}
	var body CreateProductRequest
	raw, err := decodeJSONRequest(w, r, &body)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	description := ""
	if body.Description != nil {
		description = *body.Description
	}
	mutation, err := mutationFromRequest(r, actor, "/api/v1/products", raw)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	created, err := h.service.Create(r.Context(), productdomain.CreateRequest{
		Mutation: mutation, Slug: body.Slug, DisplayName: body.DisplayName,
		Description: description, CommandName: body.CommandName,
	})
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusCreated, productDTO(created))
}

func (h productHandlers) list(w http.ResponseWriter, r *http.Request) {
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
	request := productdomain.ListRequest{Limit: limit}
	const kind = "products"
	if encoded := r.URL.Query().Get("cursor"); encoded != "" {
		cursor, err := decodeCursor(encoded, kind)
		if err != nil {
			writeHandlerError(w, r, err)
			return
		}
		request.After = &productdomain.Cursor{Slug: cursor.Key, ID: cursor.ID, CreatedAt: cursor.CreatedAt}
	}
	page, err := h.service.List(r.Context(), actor, request)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	items := make([]Product, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, productDTO(item))
	}
	var next *PageCursor
	if page.Next != nil {
		next = encodeCursor(cursorEnvelope{
			Kind: kind, Key: page.Next.Slug, ID: page.Next.ID, CreatedAt: page.Next.CreatedAt,
		})
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, ProductPage{Items: items, NextCursor: next})
}

func (h productHandlers) get(w http.ResponseWriter, r *http.Request) {
	actor, ok := identity.ActorFromContext(r.Context())
	if !ok {
		writeAuthenticationError(w, r, identity.ErrInvalidToken)
		return
	}
	item, err := h.service.Get(r.Context(), actor, chi.URLParam(r, "product"))
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, productDTO(item))
}

func (h productHandlers) rotateInstallKey(w http.ResponseWriter, r *http.Request) {
	actor, ok := identity.ActorFromContext(r.Context())
	if !ok {
		writeAuthenticationError(w, r, identity.ErrInvalidToken)
		return
	}
	slug := chi.URLParam(r, "product")
	canonicalResource := "/api/v1/products/" + slug + "/install-key/rotate"
	mutation, err := mutationFromRequest(r, actor, canonicalResource, nil)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	item, err := h.service.RotateInstallKey(r.Context(), productdomain.RotateInstallKeyRequest{
		Mutation: mutation,
		Slug:     slug,
	})
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, productDTO(item))
}

func productDTO(item productdomain.Product) Product {
	platforms := make([]ProductPlatform, 0, len(item.Platforms))
	for _, platform := range item.Platforms {
		platforms = append(platforms, ProductPlatform{
			Os:       platform.OS,
			Arch:     platform.Arch,
			Variant:  platform.Variant,
			Strategy: ProductPlatformStrategy(platform.Strategy),
			Format:   ProductPlatformFormat(platform.Format),
		})
	}
	return Product{
		Id: item.ID, Slug: item.Slug, DisplayName: item.DisplayName, Description: item.Description,
		CommandName: item.CommandName, Repository: item.RepositoryKey, Package: item.PackageName,
		InstallKey: item.InstallKey, CurrentVersion: item.CurrentStableVersion,
		PublishedAt: item.CurrentStablePublishedAt, Platforms: platforms,
		CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}
}
