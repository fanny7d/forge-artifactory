package api

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"regexp"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	identity "superfan.myasustor.com/fanchao/artifact-repository/internal/auth"
	releasedomain "superfan.myasustor.com/fanchao/artifact-repository/internal/release"
)

var (
	validCoordinate         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
	validOptionalCoordinate = regexp.MustCompile(`^[A-Za-z0-9._+-]{0,64}$`)
)

type DraftService interface {
	Create(context.Context, releasedomain.CreateDraftRequest) (releasedomain.Release, error)
	Get(context.Context, identity.Actor, string, string, string) (releasedomain.Release, error)
	List(context.Context, identity.Actor, string, string, releasedomain.ReleaseListRequest) (releasedomain.ReleasePage, error)
	AddArtifact(context.Context, releasedomain.AddArtifactRequest) (releasedomain.ReleaseArtifact, error)
	RemoveArtifact(context.Context, releasedomain.RemoveArtifactRequest) (releasedomain.RemoveArtifactResult, error)
	Cancel(context.Context, releasedomain.CancelDraftRequest) (releasedomain.CancelDraftResult, error)
}

type ReleasePublisher interface {
	Publish(context.Context, releasedomain.PublishCommand) (releasedomain.PublishedRelease, error)
	GetManifest(context.Context, identity.Actor, string, string, string) (releasedomain.SignedManifest, error)
}

type releaseHandlers struct {
	service   DraftService
	publisher ReleasePublisher
}

func registerDraftRoutes(router chi.Router, service DraftService) {
	if service == nil {
		return
	}
	handlers := releaseHandlers{service: service}
	base := "/repositories/{repo}/packages/{package}/releases"
	router.Post(base, handlers.create)
	router.Get(base, handlers.list)
	router.Get(base+"/{version}", handlers.get)
	router.Delete(base+"/{version}", handlers.cancel)
	router.Post(base+"/{version}/artifacts", handlers.addArtifact)
	router.Delete(base+"/{version}/artifacts/{releaseArtifactId}", handlers.removeArtifact)
}

func registerPublishRoutes(router chi.Router, service ReleasePublisher) {
	if service == nil {
		return
	}
	handlers := releaseHandlers{publisher: service}
	base := "/repositories/{repo}/packages/{package}/releases/{version}"
	router.Post(base+"/publish", handlers.publish)
	router.Get(base+"/manifest", handlers.manifest)
}

func (h releaseHandlers) create(w http.ResponseWriter, r *http.Request) {
	actor, repositoryKey, packageName, ok := releaseScope(w, r, false)
	if !ok {
		return
	}
	var body CreateReleaseRequest
	raw, err := decodeJSONRequest(w, r, &body)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	if !releasedomain.ValidVersion(body.Version) {
		writeHandlerError(w, r, releasedomain.ErrInvalidRequest)
		return
	}
	canonicalResource := releaseCollectionResource(repositoryKey, packageName)
	mutation, err := mutationFromRequest(r, actor, canonicalResource, raw)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	created, err := h.service.Create(r.Context(), releasedomain.CreateDraftRequest{
		Mutation:      mutation,
		RepositoryKey: repositoryKey,
		PackageName:   packageName,
		Version:       body.Version,
	})
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, releaseDTO(created))
}

func (h releaseHandlers) list(w http.ResponseWriter, r *http.Request) {
	actor, repositoryKey, packageName, ok := releaseScope(w, r, false)
	if !ok {
		return
	}
	limit, err := listLimit(r)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	request := releasedomain.ReleaseListRequest{Limit: limit}
	kind := "releases:" + repositoryKey + ":" + packageName
	if encoded := r.URL.Query().Get("cursor"); encoded != "" {
		cursor, err := decodeCursor(encoded, kind)
		if err != nil {
			writeHandlerError(w, r, err)
			return
		}
		request.After = &releasedomain.ReleaseCursor{CreatedAt: cursor.CreatedAt, ID: cursor.ID}
	}
	page, err := h.service.List(r.Context(), actor, repositoryKey, packageName, request)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	items := make([]Release, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, releaseDTO(item))
	}
	writeJSON(w, http.StatusOK, ReleasePage{Items: items, NextCursor: encodeReleaseCursor(kind, page.Next)})
}

func (h releaseHandlers) get(w http.ResponseWriter, r *http.Request) {
	actor, repositoryKey, packageName, ok := releaseScope(w, r, true)
	if !ok {
		return
	}
	version := chi.URLParam(r, "version")
	item, err := h.service.Get(r.Context(), actor, repositoryKey, packageName, version)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, releaseDTO(item))
}

func (h releaseHandlers) cancel(w http.ResponseWriter, r *http.Request) {
	actor, repositoryKey, packageName, ok := releaseScope(w, r, true)
	if !ok {
		return
	}
	version := chi.URLParam(r, "version")
	canonicalResource := releaseCollectionResource(repositoryKey, packageName) + "/" + version
	mutation, err := mutationFromRequest(r, actor, canonicalResource, nil)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	if _, err := h.service.Cancel(r.Context(), releasedomain.CancelDraftRequest{
		Mutation:      mutation,
		RepositoryKey: repositoryKey,
		PackageName:   packageName,
		Version:       version,
	}); err != nil {
		writeHandlerError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h releaseHandlers) addArtifact(w http.ResponseWriter, r *http.Request) {
	actor, repositoryKey, packageName, ok := releaseScope(w, r, true)
	if !ok {
		return
	}
	version := chi.URLParam(r, "version")
	var body AddReleaseArtifactRequest
	raw, err := decodeJSONRequest(w, r, &body)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	variant := ""
	if body.Variant != nil {
		variant = *body.Variant
	}
	role := ""
	if body.Role != nil {
		role = *body.Role
	}
	if body.ArtifactPath == "" || len(body.ArtifactPath) > 1024 ||
		!validCoordinate.MatchString(body.Os) || !validCoordinate.MatchString(body.Arch) ||
		!validOptionalCoordinate.MatchString(variant) || !validOptionalCoordinate.MatchString(role) {
		writeHandlerError(w, r, releasedomain.ErrInvalidRequest)
		return
	}
	install, err := installSpecFromDTO(body.Install)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	canonicalResource := releaseCollectionResource(repositoryKey, packageName) + "/" + version + "/artifacts"
	mutation, err := mutationFromRequest(r, actor, canonicalResource, raw)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	created, err := h.service.AddArtifact(r.Context(), releasedomain.AddArtifactRequest{
		Mutation:      mutation,
		RepositoryKey: repositoryKey,
		PackageName:   packageName,
		Version:       version,
		ArtifactPath:  body.ArtifactPath,
		OS:            body.Os,
		Arch:          body.Arch,
		Variant:       variant,
		Role:          role,
		Install:       install,
	})
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, releaseArtifactDTO(created))
}

func (h releaseHandlers) removeArtifact(w http.ResponseWriter, r *http.Request) {
	actor, repositoryKey, packageName, ok := releaseScope(w, r, true)
	if !ok {
		return
	}
	version := chi.URLParam(r, "version")
	releaseArtifactID, err := uuid.Parse(chi.URLParam(r, "releaseArtifactId"))
	if err != nil {
		writeHandlerError(w, r, releasedomain.ErrInvalidRequest)
		return
	}
	canonicalResource := releaseCollectionResource(repositoryKey, packageName) + "/" + version + "/artifacts/" + releaseArtifactID.String()
	mutation, err := mutationFromRequest(r, actor, canonicalResource, nil)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	if _, err := h.service.RemoveArtifact(r.Context(), releasedomain.RemoveArtifactRequest{
		Mutation:          mutation,
		RepositoryKey:     repositoryKey,
		PackageName:       packageName,
		Version:           version,
		ReleaseArtifactID: releaseArtifactID,
	}); err != nil {
		writeHandlerError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h releaseHandlers) publish(w http.ResponseWriter, r *http.Request) {
	actor, repositoryKey, packageName, ok := releaseScope(w, r, true)
	if !ok {
		return
	}
	if r.ContentLength > 0 {
		writeHandlerError(w, r, releasedomain.ErrInvalidRequest)
		return
	}
	if r.Body != nil {
		content, err := io.ReadAll(io.LimitReader(r.Body, 1))
		if err != nil || len(content) != 0 {
			writeHandlerError(w, r, releasedomain.ErrInvalidRequest)
			return
		}
	}
	version := chi.URLParam(r, "version")
	canonicalResource := releaseCollectionResource(repositoryKey, packageName) + "/" + version + "/publish"
	mutation, err := mutationFromRequest(r, actor, canonicalResource, nil)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	published, err := h.publisher.Publish(r.Context(), releasedomain.PublishCommand{
		Mutation:      mutation,
		RepositoryKey: repositoryKey,
		PackageName:   packageName,
		Version:       version,
	})
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, releaseDTO(published.Release))
}

func (h releaseHandlers) manifest(w http.ResponseWriter, r *http.Request) {
	actor, repositoryKey, packageName, ok := releaseScope(w, r, true)
	if !ok {
		return
	}
	manifest, err := h.publisher.GetManifest(
		r.Context(),
		actor,
		repositoryKey,
		packageName,
		chi.URLParam(r, "version"),
	)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ReleaseManifest{
		Version:        manifest.Version,
		Manifest:       base64.RawURLEncoding.EncodeToString(manifest.Manifest),
		ManifestSha256: manifest.ManifestSHA256,
		KeyId:          manifest.KeyID,
		Signature:      base64.RawURLEncoding.EncodeToString(manifest.Signature),
	})
}

func releaseScope(w http.ResponseWriter, r *http.Request, requireVersion bool) (identity.Actor, string, string, bool) {
	actor, repositoryKey, ok := scopedActorAndRepository(w, r)
	if !ok {
		return identity.Actor{}, "", "", false
	}
	packageName := chi.URLParam(r, "package")
	if !validName.MatchString(packageName) {
		writeHandlerError(w, r, releasedomain.ErrInvalidRequest)
		return identity.Actor{}, "", "", false
	}
	if requireVersion && !releasedomain.ValidVersion(chi.URLParam(r, "version")) {
		writeHandlerError(w, r, releasedomain.ErrInvalidRequest)
		return identity.Actor{}, "", "", false
	}
	return actor, repositoryKey, packageName, true
}

func releaseCollectionResource(repositoryKey, packageName string) string {
	return "/api/v1/repositories/" + repositoryKey + "/packages/" + packageName + "/releases"
}

func releaseDTO(item releasedomain.Release) Release {
	artifacts := make([]ReleaseArtifact, 0, len(item.Artifacts))
	for _, artifact := range item.Artifacts {
		artifacts = append(artifacts, releaseArtifactDTO(artifact))
	}
	return Release{
		Id:          item.ID,
		Repository:  item.RepositoryKey,
		Package:     item.PackageName,
		Version:     item.Version,
		State:       ReleaseState(item.State),
		Artifacts:   artifacts,
		PublishedAt: item.PublishedAt,
		FailureCode: item.FailureCode,
		CreatedAt:   item.CreatedAt,
	}
}

func releaseArtifactDTO(item releasedomain.ReleaseArtifact) ReleaseArtifact {
	return ReleaseArtifact{
		Id:       item.ID,
		Artifact: artifactDTO(item.Artifact),
		Os:       item.OS,
		Arch:     item.Arch,
		Variant:  item.Variant,
		Role:     item.Role,
		Install:  installSpecDTO(item.Install),
	}
}

func installSpecFromDTO(item *InstallSpec) (*releasedomain.InstallSpec, error) {
	if item == nil {
		return nil, nil
	}
	entrypoint := ""
	if item.Entrypoint != nil {
		entrypoint = *item.Entrypoint
	}
	hooks := []releasedomain.InstallHook{}
	if item.Hooks != nil {
		hooks = make([]releasedomain.InstallHook, 0, len(*item.Hooks))
		for _, hook := range *item.Hooks {
			arguments := []string{}
			if hook.Args != nil {
				arguments = append(arguments, (*hook.Args)...)
			}
			hooks = append(hooks, releasedomain.InstallHook{
				Phase:          releasedomain.HookPhase(hook.Phase),
				Path:           hook.Path,
				Args:           arguments,
				TimeoutSeconds: hook.TimeoutSeconds,
			})
		}
	}
	spec := &releasedomain.InstallSpec{
		Strategy:   releasedomain.InstallStrategy(item.Strategy),
		Format:     releasedomain.InstallFormat(item.Format),
		Entrypoint: entrypoint,
		Mode:       item.Mode,
		Hooks:      hooks,
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return spec, nil
}

func installSpecDTO(item *releasedomain.InstallSpec) *InstallSpec {
	if item == nil {
		return nil
	}
	var entrypoint *string
	if item.Entrypoint != "" {
		value := item.Entrypoint
		entrypoint = &value
	}
	var hooks *[]InstallHook
	if len(item.Hooks) > 0 {
		values := make([]InstallHook, 0, len(item.Hooks))
		for _, hook := range item.Hooks {
			var arguments *[]string
			if len(hook.Args) > 0 {
				copied := append([]string(nil), hook.Args...)
				arguments = &copied
			}
			values = append(values, InstallHook{
				Phase:          InstallHookPhase(hook.Phase),
				Path:           hook.Path,
				Args:           arguments,
				TimeoutSeconds: hook.TimeoutSeconds,
			})
		}
		hooks = &values
	}
	return &InstallSpec{
		Strategy:   InstallSpecStrategy(item.Strategy),
		Format:     InstallSpecFormat(item.Format),
		Entrypoint: entrypoint,
		Mode:       item.Mode,
		Hooks:      hooks,
	}
}

func artifactDTO(item releasedomain.Artifact) Artifact {
	return Artifact{
		Id:         item.ID,
		Repository: item.RepositoryKey,
		Path:       item.Path,
		Filename:   item.Filename,
		MediaType:  item.MediaType,
		Size:       item.Size,
		Sha256:     item.SHA256,
		Properties: item.Properties,
		CreatedBy:  item.CreatedBy,
		CreatedAt:  item.CreatedAt,
	}
}

func encodeReleaseCursor(kind string, cursor *releasedomain.ReleaseCursor) *PageCursor {
	if cursor == nil {
		return nil
	}
	return encodeCursor(cursorEnvelope{Kind: kind, CreatedAt: cursor.CreatedAt, ID: cursor.ID})
}
