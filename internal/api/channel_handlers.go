package api

import (
	"context"
	"encoding/base64"
	"net/http"

	"github.com/go-chi/chi/v5"

	identity "superfan.myasustor.com/fanchao/artifact-repository/internal/auth"
	channeldomain "superfan.myasustor.com/fanchao/artifact-repository/internal/channel"
)

type ChannelService interface {
	Promote(context.Context, channeldomain.PromoteRequest) (channeldomain.Revision, error)
	Current(context.Context, identity.Actor, string, string, string) (channeldomain.Channel, error)
	History(context.Context, identity.Actor, string, string, string, channeldomain.HistoryRequest) (channeldomain.HistoryPage, error)
	Resolve(context.Context, channeldomain.ResolveRequest) (channeldomain.Resolution, error)
}

type channelHandlers struct {
	service ChannelService
}

func registerChannelRoutes(router chi.Router, service ChannelService) {
	if service == nil {
		return
	}
	handlers := channelHandlers{service: service}
	base := "/repositories/{repo}/packages/{package}/channels/{channel}"
	router.Post(base+"/promotions", handlers.promote)
	router.Get(base, handlers.current)
	router.Get(base+"/history", handlers.history)
	router.Get(base+"/resolve", handlers.resolve)
}

func (h channelHandlers) promote(w http.ResponseWriter, r *http.Request) {
	actor, repositoryKey, packageName, channelName, ok := channelScope(w, r)
	if !ok {
		return
	}
	var body PromoteChannelRequest
	raw, err := decodeJSONRequest(w, r, &body)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	if !validVersion.MatchString(body.Version) || len(body.Reason) < 1 || len(body.Reason) > 512 {
		writeHandlerError(w, r, channeldomain.ErrInvalidRequest)
		return
	}
	canonicalResource := channelResource(repositoryKey, packageName, channelName) + "/promotions"
	mutation, err := mutationFromRequest(r, actor, canonicalResource, raw)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	revision, err := h.service.Promote(r.Context(), channeldomain.PromoteRequest{
		Mutation:      mutation,
		RepositoryKey: repositoryKey,
		PackageName:   packageName,
		ChannelName:   channelName,
		Version:       body.Version,
		Reason:        body.Reason,
	})
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, channelRevisionDTO(revision))
}

func (h channelHandlers) current(w http.ResponseWriter, r *http.Request) {
	actor, repositoryKey, packageName, channelName, ok := channelScope(w, r)
	if !ok {
		return
	}
	current, err := h.service.Current(r.Context(), actor, repositoryKey, packageName, channelName)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, channelDTO(current))
}

func (h channelHandlers) history(w http.ResponseWriter, r *http.Request) {
	actor, repositoryKey, packageName, channelName, ok := channelScope(w, r)
	if !ok {
		return
	}
	limit, err := listLimit(r)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	request := channeldomain.HistoryRequest{Limit: limit}
	kind := "channel-history:" + repositoryKey + ":" + packageName + ":" + channelName
	if encoded := r.URL.Query().Get("cursor"); encoded != "" {
		cursor, err := decodeCursor(encoded, kind)
		if err != nil {
			writeHandlerError(w, r, err)
			return
		}
		request.After = &channeldomain.HistoryCursor{CreatedAt: cursor.CreatedAt, ID: cursor.ID}
	}
	page, err := h.service.History(r.Context(), actor, repositoryKey, packageName, channelName, request)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	items := make([]ChannelRevision, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, channelRevisionDTO(item))
	}
	var next *PageCursor
	if page.Next != nil {
		next = encodeCursor(cursorEnvelope{Kind: kind, CreatedAt: page.Next.CreatedAt, ID: page.Next.ID})
	}
	writeJSON(w, http.StatusOK, ChannelRevisionPage{Items: items, NextCursor: next})
}

func (h channelHandlers) resolve(w http.ResponseWriter, r *http.Request) {
	actor, repositoryKey, packageName, channelName, ok := channelScope(w, r)
	if !ok {
		return
	}
	osValue := r.URL.Query().Get("os")
	arch := r.URL.Query().Get("arch")
	variant := r.URL.Query().Get("variant")
	role := r.URL.Query().Get("role")
	if !validCoordinate.MatchString(osValue) || !validCoordinate.MatchString(arch) ||
		!validOptionalCoordinate.MatchString(variant) || !validOptionalCoordinate.MatchString(role) {
		writeHandlerError(w, r, channeldomain.ErrInvalidRequest)
		return
	}
	redirect, err := optionalBoolQuery(r, "redirect")
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	resolved, err := h.service.Resolve(r.Context(), channeldomain.ResolveRequest{
		Actor:         actor,
		RepositoryKey: repositoryKey,
		PackageName:   packageName,
		ChannelName:   channelName,
		OS:            osValue,
		Arch:          arch,
		Variant:       variant,
		Role:          role,
		Redirect:      redirect,
	})
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ResolveResponse{
		Version:   resolved.Version,
		Manifest:  base64.RawURLEncoding.EncodeToString(resolved.Manifest),
		KeyId:     resolved.KeyID,
		Signature: base64.RawURLEncoding.EncodeToString(resolved.Signature),
		Artifact: ResolveArtifact{
			Path:    resolved.Artifact.Path,
			Os:      resolved.Artifact.OS,
			Arch:    resolved.Artifact.Arch,
			Variant: resolved.Artifact.Variant,
			Role:    resolved.Artifact.Role,
			Sha256:  resolved.Artifact.SHA256,
			Size:    resolved.Artifact.Size,
		},
		DownloadUrl: resolved.DownloadURL,
	})
}

func channelScope(w http.ResponseWriter, r *http.Request) (identity.Actor, string, string, string, bool) {
	actor, ok := identity.ActorFromContext(r.Context())
	if !ok {
		writeAuthenticationError(w, r, identity.ErrInvalidToken)
		return identity.Actor{}, "", "", "", false
	}
	repositoryKey := chi.URLParam(r, "repo")
	packageName := chi.URLParam(r, "package")
	channelName := chi.URLParam(r, "channel")
	if !validName.MatchString(repositoryKey) || !validName.MatchString(packageName) || (channelName != "candidate" && channelName != "stable") {
		writeHandlerError(w, r, channeldomain.ErrInvalidRequest)
		return identity.Actor{}, "", "", "", false
	}
	return actor, repositoryKey, packageName, channelName, true
}

func channelResource(repositoryKey, packageName, channelName string) string {
	return "/api/v1/repositories/" + repositoryKey + "/packages/" + packageName + "/channels/" + channelName
}

func channelDTO(item channeldomain.Channel) Channel {
	return Channel{
		Id:             item.ID,
		Repository:     item.RepositoryKey,
		Package:        item.PackageName,
		Name:           ChannelName(item.Name),
		CurrentVersion: item.CurrentVersion,
		CreatedAt:      item.CreatedAt,
	}
}

func channelRevisionDTO(item channeldomain.Revision) ChannelRevision {
	return ChannelRevision{
		Id:          item.ID,
		Channel:     ChannelRevisionChannel(item.Channel),
		FromVersion: item.FromVersion,
		ToVersion:   item.ToVersion,
		ActorId:     item.ActorID,
		Reason:      item.Reason,
		RequestId:   item.RequestID,
		CreatedAt:   item.CreatedAt,
	}
}
