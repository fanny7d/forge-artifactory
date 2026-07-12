package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	artifactdomain "superfan.myasustor.com/fanchao/artifact-repository/internal/artifact"
	identity "superfan.myasustor.com/fanchao/artifact-repository/internal/auth"
)

type ArtifactService interface {
	Upload(context.Context, artifactdomain.UploadRequest) (artifactdomain.Metadata, error)
	ChecksumDeploy(context.Context, artifactdomain.ChecksumDeployRequest) (artifactdomain.Metadata, error)
	Metadata(context.Context, identity.Actor, string, string) (artifactdomain.Metadata, error)
	Open(context.Context, artifactdomain.OpenRequest) (artifactdomain.OpenResult, error)
}

type artifactHandlers struct {
	service ArtifactService
}

func registerArtifactRoutes(router chi.Router, service ArtifactService) {
	if service == nil {
		return
	}
	handlers := artifactHandlers{service: service}
	router.Put("/repositories/{repo}/artifacts/*", handlers.put)
	router.Head("/repositories/{repo}/artifacts/*", handlers.head)
	router.Get("/repositories/{repo}/artifacts/*", handlers.get)
	router.Get("/repositories/{repo}/metadata/*", handlers.metadata)
}

func (h artifactHandlers) put(w http.ResponseWriter, r *http.Request) {
	actor, repositoryKey, ok := artifactScope(w, r)
	if !ok {
		return
	}
	rawPath, err := escapedWildcardPath(r, "/artifacts/")
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	properties, err := decodePropertiesHeader(r.Header.Get("X-Artifact-Properties"))
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	mediaType, err := artifactMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	checksumDeploy, err := optionalBoolHeader(r.Header.Get("X-Checksum-Deploy"))
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	checksum := r.Header.Get("X-Checksum-Sha256")
	if checksumDeploy {
		if r.ContentLength != 0 || checksum == "" {
			writeHandlerError(w, r, artifactdomain.ErrInvalidRequest)
			return
		}
		created, err := h.service.ChecksumDeploy(r.Context(), artifactdomain.ChecksumDeployRequest{
			Actor: actor, RequestID: requestIDFromContext(r.Context()), RepositoryKey: repositoryKey,
			RawPath: rawPath, SHA256: checksum, MediaType: mediaType, Properties: properties,
		})
		if err != nil {
			writeHandlerError(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, artifactMetadataDTO(created))
		return
	}
	if r.ContentLength < 0 {
		writeHandlerError(w, r, artifactdomain.ErrInvalidRequest)
		return
	}
	created, err := h.service.Upload(r.Context(), artifactdomain.UploadRequest{
		Actor: actor, RequestID: requestIDFromContext(r.Context()), RepositoryKey: repositoryKey,
		RawPath: rawPath, Body: r.Body, ContentLength: r.ContentLength, MediaType: mediaType,
		Properties: properties, ExpectedSHA256: checksum,
	})
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, artifactMetadataDTO(created))
}

func (h artifactHandlers) metadata(w http.ResponseWriter, r *http.Request) {
	actor, repositoryKey, ok := artifactScope(w, r)
	if !ok {
		return
	}
	rawPath, err := escapedWildcardPath(r, "/metadata/")
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	metadata, err := h.service.Metadata(r.Context(), actor, repositoryKey, rawPath)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, artifactMetadataDTO(metadata))
}

func (h artifactHandlers) head(w http.ResponseWriter, r *http.Request) {
	actor, repositoryKey, ok := artifactScope(w, r)
	if !ok {
		return
	}
	rawPath, err := escapedWildcardPath(r, "/artifacts/")
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	metadata, err := h.service.Metadata(r.Context(), actor, repositoryKey, rawPath)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	setArtifactHeaders(w, metadata)
	w.Header().Set("Content-Length", strconv.FormatInt(metadata.Size, 10))
	w.WriteHeader(http.StatusOK)
}

func (h artifactHandlers) get(w http.ResponseWriter, r *http.Request) {
	actor, repositoryKey, ok := artifactScope(w, r)
	if !ok {
		return
	}
	rawPath, err := escapedWildcardPath(r, "/artifacts/")
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	redirect, err := optionalBoolQuery(r, "redirect")
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	result, err := h.service.Open(r.Context(), artifactdomain.OpenRequest{
		Actor: actor, RepositoryKey: repositoryKey, RawPath: rawPath, Redirect: redirect,
	})
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	if result.RedirectURL != "" {
		w.Header().Set("Location", result.RedirectURL)
		w.WriteHeader(http.StatusTemporaryRedirect)
		return
	}
	if result.Object.Body == nil || result.Object.Seeker == nil {
		if result.Object.Body != nil {
			_ = result.Object.Body.Close()
		}
		writeHandlerError(w, r, fmt.Errorf("proxy object is not seekable"))
		return
	}
	defer func() { _ = result.Object.Body.Close() }()
	setArtifactHeaders(w, result.Metadata)
	w.Header().Set("Content-Type", result.Metadata.MediaType)
	w.Header().Set("Accept-Ranges", "bytes")
	http.ServeContent(w, r, result.Metadata.Filename, result.Metadata.CreatedAt, result.Object.Seeker)
}

func artifactScope(w http.ResponseWriter, r *http.Request) (identity.Actor, string, bool) {
	actor, ok := identity.ActorFromContext(r.Context())
	if !ok {
		writeAuthenticationError(w, r, identity.ErrInvalidToken)
		return identity.Actor{}, "", false
	}
	repositoryKey := chi.URLParam(r, "repo")
	if !validName.MatchString(repositoryKey) {
		writeHandlerError(w, r, artifactdomain.ErrInvalidRequest)
		return identity.Actor{}, "", false
	}
	return actor, repositoryKey, true
}

func escapedWildcardPath(r *http.Request, marker string) (string, error) {
	repositoryKey := chi.URLParam(r, "repo")
	prefix := "/api/v1/repositories/" + repositoryKey + marker
	escapedPath := r.URL.EscapedPath()
	if !strings.HasPrefix(escapedPath, prefix) || len(escapedPath) == len(prefix) {
		return "", artifactdomain.ErrInvalidRequest
	}
	return escapedPath[len(prefix):], nil
}

func decodePropertiesHeader(encoded string) (map[string]any, error) {
	if encoded == "" {
		return map[string]any{}, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(decoded) > 16<<10 || !utf8.Valid(decoded) {
		return nil, artifactdomain.ErrInvalidRequest
	}
	decoder := json.NewDecoder(strings.NewReader(string(decoded)))
	decoder.UseNumber()
	value, err := decodeUniqueJSONValue(decoder)
	if err != nil {
		return nil, artifactdomain.ErrInvalidRequest
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, artifactdomain.ErrInvalidRequest
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, artifactdomain.ErrInvalidRequest
	}
	return object, nil
}

func decodeUniqueJSONValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return token, nil
	}
	switch delimiter {
	case '{':
		object := map[string]any{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, fmt.Errorf("JSON object key is not a string")
			}
			if _, duplicate := object[key]; duplicate {
				return nil, fmt.Errorf("duplicate JSON key %q", key)
			}
			value, err := decodeUniqueJSONValue(decoder)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		if _, err := decoder.Token(); err != nil {
			return nil, err
		}
		return object, nil
	case '[':
		array := []any{}
		for decoder.More() {
			value, err := decodeUniqueJSONValue(decoder)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		if _, err := decoder.Token(); err != nil {
			return nil, err
		}
		return array, nil
	default:
		return nil, fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
}

func artifactMediaType(value string) (string, error) {
	if value == "" {
		return "application/octet-stream", nil
	}
	mediaType, parameters, err := mime.ParseMediaType(value)
	if err != nil || len(mediaType) > 255 {
		return "", artifactdomain.ErrInvalidRequest
	}
	if len(parameters) == 0 {
		return mediaType, nil
	}
	return mime.FormatMediaType(mediaType, parameters), nil
}

func optionalBoolHeader(value string) (bool, error) {
	if value == "" {
		return false, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, artifactdomain.ErrInvalidRequest
	}
	return parsed, nil
}

func optionalBoolQuery(r *http.Request, name string) (*bool, error) {
	value, present := r.URL.Query()[name]
	if !present {
		return nil, nil
	}
	if len(value) != 1 {
		return nil, artifactdomain.ErrInvalidRequest
	}
	parsed, err := strconv.ParseBool(value[0])
	if err != nil {
		return nil, artifactdomain.ErrInvalidRequest
	}
	return &parsed, nil
}

func artifactMetadataDTO(metadata artifactdomain.Metadata) Artifact {
	return Artifact{
		Id: metadata.ID, Repository: metadata.RepositoryKey, Path: metadata.Path,
		Filename: metadata.Filename, MediaType: metadata.MediaType, Size: metadata.Size,
		Sha256: metadata.SHA256, Properties: metadata.Properties,
		CreatedBy: metadata.CreatedBy, CreatedAt: metadata.CreatedAt,
	}
}

func setArtifactHeaders(w http.ResponseWriter, metadata artifactdomain.Metadata) {
	w.Header().Set("ETag", `"`+metadata.SHA256+`"`)
	w.Header().Set("X-Checksum-Sha256", metadata.SHA256)
	w.Header().Set("X-Created-At", metadata.CreatedAt.Format(time.RFC3339Nano))
}
