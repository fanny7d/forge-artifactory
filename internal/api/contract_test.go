package api

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

func TestOpenAPIContainsEveryMVPPath(t *testing.T) {
	document := loadOpenAPI(t)

	operations := []struct {
		method      string
		path        string
		operationID string
	}{
		{http.MethodGet, "/healthz", "healthz"},
		{http.MethodGet, "/readyz", "readyz"},
		{http.MethodGet, "/metrics", "metrics"},
		{http.MethodPost, "/api/v1/repositories", "createRepository"},
		{http.MethodGet, "/api/v1/repositories", "listRepositories"},
		{http.MethodGet, "/api/v1/repositories/{repo}", "getRepository"},
		{http.MethodPut, "/api/v1/repositories/{repo}/artifacts/{path}", "putArtifact"},
		{http.MethodHead, "/api/v1/repositories/{repo}/artifacts/{path}", "headArtifact"},
		{http.MethodGet, "/api/v1/repositories/{repo}/artifacts/{path}", "getArtifact"},
		{http.MethodGet, "/api/v1/repositories/{repo}/metadata/{path}", "getArtifactMetadata"},
		{http.MethodPost, "/api/v1/repositories/{repo}/packages", "createPackage"},
		{http.MethodGet, "/api/v1/repositories/{repo}/packages", "listPackages"},
		{http.MethodGet, "/api/v1/repositories/{repo}/packages/{package}", "getPackage"},
		{http.MethodPost, "/api/v1/repositories/{repo}/packages/{package}/releases", "createRelease"},
		{http.MethodGet, "/api/v1/repositories/{repo}/packages/{package}/releases", "listReleases"},
		{http.MethodGet, "/api/v1/repositories/{repo}/packages/{package}/releases/{version}", "getRelease"},
		{http.MethodDelete, "/api/v1/repositories/{repo}/packages/{package}/releases/{version}", "cancelRelease"},
		{http.MethodPost, "/api/v1/repositories/{repo}/packages/{package}/releases/{version}/artifacts", "addReleaseArtifact"},
		{http.MethodDelete, "/api/v1/repositories/{repo}/packages/{package}/releases/{version}/artifacts/{releaseArtifactId}", "removeReleaseArtifact"},
		{http.MethodPost, "/api/v1/repositories/{repo}/packages/{package}/releases/{version}/publish", "publishRelease"},
		{http.MethodGet, "/api/v1/repositories/{repo}/packages/{package}/releases/{version}/manifest", "getReleaseManifest"},
		{http.MethodPost, "/api/v1/repositories/{repo}/packages/{package}/channels/{channel}/promotions", "promoteChannel"},
		{http.MethodGet, "/api/v1/repositories/{repo}/packages/{package}/channels/{channel}", "getChannel"},
		{http.MethodGet, "/api/v1/repositories/{repo}/packages/{package}/channels/{channel}/history", "getChannelHistory"},
		{http.MethodGet, "/api/v1/repositories/{repo}/packages/{package}/channels/{channel}/resolve", "resolveChannel"},
		{http.MethodPost, "/api/v1/service-accounts", "createServiceAccount"},
		{http.MethodGet, "/api/v1/service-accounts", "listServiceAccounts"},
		{http.MethodGet, "/api/v1/service-accounts/{id}", "getServiceAccount"},
		{http.MethodPost, "/api/v1/service-accounts/{id}/tokens", "createToken"},
		{http.MethodGet, "/api/v1/service-accounts/{id}/tokens", "listTokens"},
		{http.MethodPost, "/api/v1/tokens/{id}/revoke", "revokeToken"},
		{http.MethodGet, "/api/v1/audit-events", "listAuditEvents"},
		{http.MethodGet, "/api/v1/signing-keys/{keyId}", "getSigningKey"},
	}

	for _, expected := range operations {
		t.Run(expected.operationID, func(t *testing.T) {
			pathItem := document.Paths.Find(expected.path)
			if pathItem == nil {
				t.Fatalf("path %s is missing", expected.path)
			}
			operation := pathItem.GetOperation(expected.method)
			if operation == nil {
				t.Fatalf("%s %s is missing", expected.method, expected.path)
			}
			if operation.OperationID != expected.operationID {
				t.Fatalf("operationId = %q, want %q", operation.OperationID, expected.operationID)
			}
		})
	}
}

func TestOpenAPIRequestObjectsRejectUnknownFields(t *testing.T) {
	document := loadOpenAPI(t)
	for _, name := range []string{
		"CreateRepositoryRequest",
		"CreatePackageRequest",
		"CreateReleaseRequest",
		"AddReleaseArtifactRequest",
		"PromoteChannelRequest",
		"CreateServiceAccountRequest",
		"CreateTokenRequest",
	} {
		schemaRef := document.Components.Schemas[name]
		if schemaRef == nil || schemaRef.Value == nil {
			t.Fatalf("schema %s is missing", name)
		}
		if schemaRef.Value.AdditionalProperties.Has == nil || *schemaRef.Value.AdditionalProperties.Has {
			t.Fatalf("schema %s must set additionalProperties: false", name)
		}
	}
}

func TestOpenAPIDeleteMutationsAcceptIdempotencyKey(t *testing.T) {
	document := loadOpenAPI(t)
	for _, path := range []string{
		"/api/v1/repositories/{repo}/packages/{package}/releases/{version}",
		"/api/v1/repositories/{repo}/packages/{package}/releases/{version}/artifacts/{releaseArtifactId}",
	} {
		operation := document.Paths.Find(path).Delete
		if operation == nil {
			t.Fatalf("DELETE %s is missing", path)
		}
		found := false
		for _, parameter := range operation.Parameters {
			if parameter.Ref == "#/components/parameters/IdempotencyKey" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("DELETE %s does not accept Idempotency-Key", path)
		}
	}
}

func TestOpenAPIAuthenticatedOperationsDocumentRateLimits(t *testing.T) {
	document := loadOpenAPI(t)
	methods := []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete}
	for path, pathItem := range document.Paths.Map() {
		if !strings.HasPrefix(path, "/api/v1/") {
			continue
		}
		for _, method := range methods {
			operation := pathItem.GetOperation(method)
			if operation == nil {
				continue
			}
			response := operation.Responses.Value("429")
			if response == nil || response.Ref != "#/components/responses/TooManyRequests" {
				t.Errorf("%s %s does not document the shared 429 response", method, path)
			}
		}
	}
}

func loadOpenAPI(t *testing.T) *openapi3.T {
	t.Helper()
	path := filepath.Join("..", "..", "openapi", "openapi.yaml")
	document, err := openapi3.NewLoader().LoadFromFile(path)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	if err := document.Validate(context.Background()); err != nil {
		t.Fatalf("validate OpenAPI: %v", err)
	}
	return document
}
