package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	identity "superfan.myasustor.com/fanchao/artifact-repository/internal/auth"
	channeldomain "superfan.myasustor.com/fanchao/artifact-repository/internal/channel"
)

func TestChannelHandlersExposePromotionHistoryCurrentAndResolve(t *testing.T) {
	authenticator := &identityServiceStub{actor: adminAPIActor()}
	createdAt := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	fromVersion := "1.0.0"
	currentVersion := "1.1.0"
	service := &channelServiceStub{
		promoteResult: channeldomain.Revision{
			ID: uuid.MustParse("11111111-aaaa-4bbb-8ccc-222222222222"), Channel: "candidate",
			FromVersion: &fromVersion, ToVersion: currentVersion, ActorID: authenticator.actor.TokenID,
			Reason: "CI promotion", RequestID: "request-promote", CreatedAt: createdAt,
		},
		currentResult: channeldomain.Channel{
			ID: uuid.MustParse("22222222-bbbb-4ccc-8ddd-333333333333"), RepositoryKey: "releases",
			PackageName: "edgecli", Name: "candidate", CurrentVersion: &currentVersion, CreatedAt: createdAt,
		},
		historyResult: channeldomain.HistoryPage{
			Items: []channeldomain.Revision{{
				ID: uuid.MustParse("33333333-cccc-4ddd-8eee-444444444444"), Channel: "candidate",
				ToVersion: currentVersion, ActorID: authenticator.actor.TokenID, Reason: "CI promotion",
				RequestID: "request-promote", CreatedAt: createdAt,
			}},
			Next: &channeldomain.HistoryCursor{CreatedAt: createdAt, ID: uuid.MustParse("33333333-cccc-4ddd-8eee-444444444444")},
		},
		resolveResult: channeldomain.Resolution{
			Version: "1.1.0", Manifest: []byte{0xfb, 0xff, 0x00}, KeyID: "ed25519:key",
			Signature: []byte{0xfa, 0x10, 0xff},
			Artifact: channeldomain.ResolvedArtifact{
				Path: "linux/arm64/edgecli", OS: "linux", Arch: "arm64", Role: "binary",
				SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Size: 7,
			},
			DownloadURL: "/api/v1/repositories/releases/artifacts/linux/arm64/edgecli?redirect=false",
		},
	}
	handler := NewServer(Dependencies{
		Readiness:     &readinessProbe{},
		Authenticator: authenticator,
		Channels:      service,
	})

	promote := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/repositories/releases/packages/edgecli/channels/candidate/promotions",
		bytes.NewBufferString(`{"version":"1.1.0","reason":"CI promotion"}`),
	)
	promote.Header.Set("Authorization", "Bearer ar1.valid")
	promote.Header.Set("Content-Type", "application/json")
	promote.Header.Set("Idempotency-Key", "promote-1.1.0")
	promoteResponse := httptest.NewRecorder()
	handler.ServeHTTP(promoteResponse, promote)
	if promoteResponse.Code != http.StatusOK {
		t.Fatalf("promote status = %d, body = %s", promoteResponse.Code, promoteResponse.Body.String())
	}
	if service.promoteRequest.Mutation.CanonicalResource != "/api/v1/repositories/releases/packages/edgecli/channels/candidate/promotions" || service.promoteRequest.Version != "1.1.0" {
		t.Fatalf("promote request = %+v", service.promoteRequest)
	}

	requests := []string{
		"/api/v1/repositories/releases/packages/edgecli/channels/candidate",
		"/api/v1/repositories/releases/packages/edgecli/channels/candidate/history?limit=1",
		"/api/v1/repositories/releases/packages/edgecli/channels/candidate/resolve?os=linux&arch=arm64&role=binary&redirect=false",
	}
	responses := make([]*httptest.ResponseRecorder, 0, len(requests))
	for _, path := range requests {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer ar1.valid")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, body = %s", path, response.Code, response.Body.String())
		}
		responses = append(responses, response)
	}
	var history ChannelRevisionPage
	if err := json.Unmarshal(responses[1].Body.Bytes(), &history); err != nil {
		t.Fatalf("decode history: %v", err)
	}
	if len(history.Items) != 1 || history.NextCursor == nil || service.historyRequest.Limit != 1 {
		t.Fatalf("history = %+v, request = %+v", history, service.historyRequest)
	}
	var resolved ResolveResponse
	if err := json.Unmarshal(responses[2].Body.Bytes(), &resolved); err != nil {
		t.Fatalf("decode resolve: %v", err)
	}
	if resolved.Manifest != base64.RawURLEncoding.EncodeToString(service.resolveResult.Manifest) || resolved.Signature != base64.RawURLEncoding.EncodeToString(service.resolveResult.Signature) {
		t.Fatalf("resolve response = %+v", resolved)
	}
	if service.resolveRequest.Redirect == nil || *service.resolveRequest.Redirect || service.resolveRequest.Role != "binary" {
		t.Fatalf("resolve request = %+v", service.resolveRequest)
	}
}

type channelServiceStub struct {
	promoteRequest PromoteChannelRequestCapture
	promoteResult  channeldomain.Revision
	currentResult  channeldomain.Channel
	historyRequest channeldomain.HistoryRequest
	historyResult  channeldomain.HistoryPage
	resolveRequest channeldomain.ResolveRequest
	resolveResult  channeldomain.Resolution
}

type PromoteChannelRequestCapture = channeldomain.PromoteRequest

func (s *channelServiceStub) Promote(_ context.Context, request channeldomain.PromoteRequest) (channeldomain.Revision, error) {
	s.promoteRequest = request
	return s.promoteResult, nil
}

func (s *channelServiceStub) Current(context.Context, identity.Actor, string, string, string) (channeldomain.Channel, error) {
	return s.currentResult, nil
}

func (s *channelServiceStub) History(_ context.Context, _ identity.Actor, _, _, _ string, request channeldomain.HistoryRequest) (channeldomain.HistoryPage, error) {
	s.historyRequest = request
	return s.historyResult, nil
}

func (s *channelServiceStub) Resolve(_ context.Context, request channeldomain.ResolveRequest) (channeldomain.Resolution, error) {
	s.resolveRequest = request
	return s.resolveResult, nil
}
