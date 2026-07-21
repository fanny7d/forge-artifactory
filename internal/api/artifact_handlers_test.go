package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	artifactdomain "superfan.myasustor.com/fanchao/artifact-repository/internal/artifact"
	identity "superfan.myasustor.com/fanchao/artifact-repository/internal/auth"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/storage"
)

func TestPutArtifactPassesWildcardPathAndStrictProperties(t *testing.T) {
	authenticator := &identityServiceStub{actor: adminAPIActor()}
	service := &artifactServiceStub{uploadResult: apiArtifactMetadata()}
	handler := NewServer(Dependencies{
		Readiness: &readinessProbe{}, Authenticator: authenticator, Artifacts: service,
	})
	properties := base64.RawURLEncoding.EncodeToString([]byte(`{"kind":"binary"}`))
	request := httptest.NewRequest(
		http.MethodPut,
		"/api/v1/repositories/source/artifacts/linux/arm64/edgecli",
		bytes.NewReader([]byte("edgecli")),
	)
	request.Header.Set("Authorization", "Bearer ar1.valid")
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("X-Artifact-Properties", properties)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if service.uploadRequest.RepositoryKey != "source" || service.uploadRequest.RawPath != "linux/arm64/edgecli" {
		t.Fatalf("Upload() request = %+v", service.uploadRequest)
	}
	if string(service.uploadBody) != "edgecli" || service.uploadRequest.Properties["kind"] != "binary" {
		t.Fatalf("Upload() body/properties = %q, %#v", service.uploadBody, service.uploadRequest.Properties)
	}

	duplicate := base64.RawURLEncoding.EncodeToString([]byte(`{"kind":"binary","kind":"archive"}`))
	bad := httptest.NewRequest(http.MethodPut, "/api/v1/repositories/source/artifacts/duplicate", bytes.NewReader(nil))
	bad.Header.Set("Authorization", "Bearer ar1.valid")
	bad.Header.Set("Content-Type", "application/octet-stream")
	bad.Header.Set("X-Artifact-Properties", duplicate)
	badResponse := httptest.NewRecorder()
	handler.ServeHTTP(badResponse, bad)
	if badResponse.Code != http.StatusBadRequest {
		t.Fatalf("duplicate properties status = %d, body = %s", badResponse.Code, badResponse.Body.String())
	}
	if service.uploadCalls != 1 {
		t.Fatalf("Upload() calls = %d, want 1", service.uploadCalls)
	}
}

func TestChecksumDeployAndHeadArtifactMapHeaders(t *testing.T) {
	authenticator := &identityServiceStub{actor: adminAPIActor()}
	metadata := apiArtifactMetadata()
	service := &artifactServiceStub{checksumResult: metadata, metadataResult: metadata}
	handler := NewServer(Dependencies{
		Readiness: &readinessProbe{}, Authenticator: authenticator, Artifacts: service,
	})
	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	request := httptest.NewRequest(http.MethodPut, "/api/v1/repositories/source/artifacts/checksum/deploy", bytes.NewReader(nil))
	request.Header.Set("Authorization", "Bearer ar1.valid")
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("X-Checksum-Deploy", "true")
	request.Header.Set("X-Checksum-Sha256", sha)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || service.checksumRequest.SHA256 != sha {
		t.Fatalf("checksum status = %d, request = %+v", response.Code, service.checksumRequest)
	}

	head := httptest.NewRequest(http.MethodHead, "/api/v1/repositories/source/artifacts/checksum/deploy", nil)
	head.Header.Set("Authorization", "Bearer ar1.valid")
	headResponse := httptest.NewRecorder()
	handler.ServeHTTP(headResponse, head)
	if headResponse.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d", headResponse.Code)
	}
	if headResponse.Header().Get("X-Checksum-Sha256") != metadata.SHA256 || headResponse.Header().Get("Content-Length") != "7" || headResponse.Header().Get("X-Created-At") == "" {
		t.Fatalf("HEAD headers = %#v", headResponse.Header())
	}
}

func TestGetArtifactUsesServeContentForRangesAndRedirects(t *testing.T) {
	authenticator := &identityServiceStub{actor: adminAPIActor()}
	metadata := apiArtifactMetadata()
	metadata.Filename = "payload.html"
	metadata.MediaType = "text/html"
	content := []byte("abcdefg")
	seekable := &seekReadCloser{Reader: bytes.NewReader(content)}
	multipartSeekable := &seekReadCloser{Reader: bytes.NewReader(content)}
	service := &artifactServiceStub{openResults: []artifactdomain.OpenResult{
		{Metadata: metadata, Object: storage.Object{
			Body: seekable, Seeker: seekable,
			Info: storage.ObjectInfo{Size: int64(len(content))},
		}},
		{Metadata: metadata, Object: storage.Object{
			Body: multipartSeekable, Seeker: multipartSeekable,
			Info: storage.ObjectInfo{Size: int64(len(content))},
		}},
		{Metadata: metadata, RedirectURL: "https://downloads.example.test/object"},
	}}
	handler := NewServer(Dependencies{
		Readiness: &readinessProbe{}, Authenticator: authenticator, Artifacts: service,
	})

	proxy := httptest.NewRequest(http.MethodGet, "/api/v1/repositories/source/artifacts/linux/arm64/edgecli?redirect=false", nil)
	proxy.Header.Set("Authorization", "Bearer ar1.valid")
	proxy.Header.Set("Range", "bytes=1-3")
	proxyResponse := httptest.NewRecorder()
	handler.ServeHTTP(proxyResponse, proxy)
	if proxyResponse.Code != http.StatusPartialContent || proxyResponse.Body.String() != "bcd" {
		t.Fatalf("proxy status = %d, body = %q", proxyResponse.Code, proxyResponse.Body.String())
	}
	if proxyResponse.Header().Get("Content-Range") != "bytes 1-3/7" {
		t.Fatalf("Content-Range = %q", proxyResponse.Header().Get("Content-Range"))
	}
	if proxyResponse.Header().Get("Content-Disposition") != "attachment; filename=payload.html" {
		t.Fatalf("Content-Disposition = %q", proxyResponse.Header().Get("Content-Disposition"))
	}
	if proxyResponse.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q", proxyResponse.Header().Get("X-Content-Type-Options"))
	}

	multipart := httptest.NewRequest(http.MethodGet, "/api/v1/repositories/source/artifacts/linux/arm64/edgecli?redirect=false", nil)
	multipart.Header.Set("Authorization", "Bearer ar1.valid")
	multipart.Header.Set("Range", "bytes=0-0,2-2")
	multipartResponse := httptest.NewRecorder()
	handler.ServeHTTP(multipartResponse, multipart)
	if multipartResponse.Code != http.StatusPartialContent || !strings.HasPrefix(multipartResponse.Header().Get("Content-Type"), "multipart/byteranges;") {
		t.Fatalf("multipart status = %d, headers = %#v", multipartResponse.Code, multipartResponse.Header())
	}
	if !bytes.Contains(multipartResponse.Body.Bytes(), []byte("a")) || !bytes.Contains(multipartResponse.Body.Bytes(), []byte("c")) {
		t.Fatalf("multipart body = %q", multipartResponse.Body.String())
	}

	redirect := httptest.NewRequest(http.MethodGet, "/api/v1/repositories/source/artifacts/linux/arm64/edgecli", nil)
	redirect.Header.Set("Authorization", "Bearer ar1.valid")
	redirectResponse := httptest.NewRecorder()
	handler.ServeHTTP(redirectResponse, redirect)
	if redirectResponse.Code != http.StatusTemporaryRedirect || redirectResponse.Header().Get("Location") != "https://downloads.example.test/object" {
		t.Fatalf("redirect status = %d, headers = %#v", redirectResponse.Code, redirectResponse.Header())
	}
}

func apiArtifactMetadata() artifactdomain.Metadata {
	return artifactdomain.Metadata{
		ID:            uuid.MustParse("11111111-2222-4333-8444-555555555555"),
		RepositoryID:  uuid.MustParse("22222222-3333-4444-8555-666666666666"),
		RepositoryKey: "source", Path: "linux/arm64/edgecli", Filename: "edgecli",
		MediaType: "application/octet-stream", Size: 7,
		SHA256:     "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Properties: map[string]any{"kind": "binary"},
		CreatedBy:  uuid.MustParse("33333333-4444-4555-8666-777777777777"),
		CreatedAt:  time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC),
	}
}

type artifactServiceStub struct {
	uploadRequest   artifactdomain.UploadRequest
	uploadBody      []byte
	uploadResult    artifactdomain.Metadata
	uploadCalls     int
	checksumRequest artifactdomain.ChecksumDeployRequest
	checksumResult  artifactdomain.Metadata
	metadataResult  artifactdomain.Metadata
	openResults     []artifactdomain.OpenResult
}

func (s *artifactServiceStub) Upload(_ context.Context, request artifactdomain.UploadRequest) (artifactdomain.Metadata, error) {
	s.uploadCalls++
	s.uploadRequest = request
	s.uploadBody, _ = io.ReadAll(request.Body)
	return s.uploadResult, nil
}

func (s *artifactServiceStub) ChecksumDeploy(_ context.Context, request artifactdomain.ChecksumDeployRequest) (artifactdomain.Metadata, error) {
	s.checksumRequest = request
	return s.checksumResult, nil
}

func (s *artifactServiceStub) Metadata(context.Context, identity.Actor, string, string) (artifactdomain.Metadata, error) {
	return s.metadataResult, nil
}

func (s *artifactServiceStub) Open(context.Context, artifactdomain.OpenRequest) (artifactdomain.OpenResult, error) {
	result := s.openResults[0]
	s.openResults = s.openResults[1:]
	return result, nil
}

type seekReadCloser struct {
	*bytes.Reader
}

func (*seekReadCloser) Close() error { return nil }
