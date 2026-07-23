package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	identity "superfan.myasustor.com/fanchao/artifact-repository/internal/auth"
	productdomain "superfan.myasustor.com/fanchao/artifact-repository/internal/product"
)

func TestProductHandlersCreateGetAndRotate(t *testing.T) {
	createdAt := time.Date(2026, 7, 23, 9, 0, 0, 0, time.UTC)
	publishedAt := createdAt.Add(time.Hour)
	currentVersion := "1.2.3"
	initialKey := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	rotatedKey := uuid.MustParse("66666666-7777-4888-8999-aaaaaaaaaaaa")
	productID := uuid.MustParse("bbbbbbbb-cccc-4ddd-8eee-ffffffffffff")
	product := productdomain.Product{
		ID: productID, Slug: "edgecli",
		PackageID:     uuid.MustParse("cccccccc-dddd-4eee-8fff-000000000000"),
		RepositoryID:  uuid.MustParse("dddddddd-eeee-4fff-8000-111111111111"),
		RepositoryKey: "cli-releases", PackageName: "edgecli",
		DisplayName: "Edge CLI", Description: "Manage edge devices", CommandName: "edgectl",
		InstallKey: initialKey, CurrentStableVersion: &currentVersion,
		CurrentStablePublishedAt: &publishedAt,
		Platforms: []productdomain.Platform{{
			OS: "linux", Arch: "arm64", Variant: "musl",
			Strategy: "bundle", Format: "tar.gz",
		}},
		CreatedAt: createdAt, UpdatedAt: publishedAt,
	}
	authenticator := &identityServiceStub{actor: adminAPIActor()}
	products := &productServiceStub{
		createResult: product,
		getResult:    product,
		rotateResult: product,
	}
	products.rotateResult.InstallKey = rotatedKey
	handler := NewServer(Dependencies{
		Readiness: &readinessProbe{}, Authenticator: authenticator, Products: products,
	})

	createRequest := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/products",
		bytes.NewBufferString(`{"slug":"edgecli","displayName":"Edge CLI","description":"Manage edge devices","commandName":"edgectl"}`),
	)
	createRequest.Header.Set("Authorization", "Bearer ar1.valid")
	createRequest.Header.Set("Content-Type", "application/json")
	createRequest.Header.Set("Idempotency-Key", "create-edgecli")
	createResponse := httptest.NewRecorder()
	handler.ServeHTTP(createResponse, createRequest)
	if createResponse.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", createResponse.Code, createResponse.Body.String())
	}
	if products.createRequest.Slug != "edgecli" ||
		products.createRequest.DisplayName != "Edge CLI" ||
		products.createRequest.Description != "Manage edge devices" ||
		products.createRequest.CommandName != "edgectl" {
		t.Fatalf("create request = %+v", products.createRequest)
	}
	if mutation := products.createRequest.Mutation; mutation.Actor.TokenID != authenticator.actor.TokenID ||
		mutation.CanonicalResource != "/api/v1/products" ||
		mutation.IdempotencyKey != "create-edgecli" ||
		mutation.Method != http.MethodPost ||
		len(mutation.Fingerprint) == 0 {
		t.Fatalf("create mutation = %+v", mutation)
	}
	assertProductDTO(t, createResponse, product, initialKey)

	getRequest := httptest.NewRequest(http.MethodGet, "/api/v1/products/edgecli", nil)
	getRequest.Header.Set("Authorization", "Bearer ar1.valid")
	getResponse := httptest.NewRecorder()
	handler.ServeHTTP(getResponse, getRequest)
	if getResponse.Code != http.StatusOK {
		t.Fatalf("get status = %d, body = %s", getResponse.Code, getResponse.Body.String())
	}
	if products.getSlug != "edgecli" || products.getActor.TokenID != authenticator.actor.TokenID {
		t.Fatalf("get actor/slug = %+v, %q", products.getActor, products.getSlug)
	}
	assertProductDTO(t, getResponse, product, initialKey)

	rotateRequest := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/products/edgecli/install-key/rotate",
		nil,
	)
	rotateRequest.Header.Set("Authorization", "Bearer ar1.valid")
	rotateRequest.Header.Set("Idempotency-Key", "rotate-edgecli-key")
	rotateResponse := httptest.NewRecorder()
	handler.ServeHTTP(rotateResponse, rotateRequest)
	if rotateResponse.Code != http.StatusOK {
		t.Fatalf("rotate status = %d, body = %s", rotateResponse.Code, rotateResponse.Body.String())
	}
	if products.rotateRequest.Slug != "edgecli" ||
		products.rotateRequest.Mutation.Actor.TokenID != authenticator.actor.TokenID ||
		products.rotateRequest.Mutation.CanonicalResource != "/api/v1/products/edgecli/install-key/rotate" ||
		products.rotateRequest.Mutation.IdempotencyKey != "rotate-edgecli-key" ||
		products.rotateRequest.Mutation.Method != http.MethodPost {
		t.Fatalf("rotate request = %+v", products.rotateRequest)
	}
	assertProductDTO(t, rotateResponse, products.rotateResult, rotatedKey)
}

func TestProductListRoundTripsCursorAndDTO(t *testing.T) {
	createdAt := time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC)
	nextID := uuid.MustParse("11111111-aaaa-4bbb-8ccc-222222222222")
	product := productdomain.Product{
		ID:   uuid.MustParse("22222222-bbbb-4ccc-8ddd-333333333333"),
		Slug: "edgecli", RepositoryKey: "cli-releases", PackageName: "edgecli",
		DisplayName: "Edge CLI", CommandName: "edgectl",
		InstallKey: uuid.MustParse("33333333-cccc-4ddd-8eee-444444444444"),
		Platforms:  []productdomain.Platform{},
		CreatedAt:  createdAt, UpdatedAt: createdAt,
	}
	authenticator := &identityServiceStub{actor: adminAPIActor()}
	products := &productServiceStub{listResults: []productdomain.Page{{
		Items: []productdomain.Product{product},
		Next:  &productdomain.Cursor{Slug: "edgecli", ID: nextID, CreatedAt: createdAt},
	}, {
		Items: []productdomain.Product{},
	}}}
	handler := NewServer(Dependencies{
		Readiness: &readinessProbe{}, Authenticator: authenticator, Products: products,
	})

	firstRequest := httptest.NewRequest(http.MethodGet, "/api/v1/products?limit=1", nil)
	firstRequest.Header.Set("Authorization", "Bearer ar1.valid")
	firstResponse := httptest.NewRecorder()
	handler.ServeHTTP(firstResponse, firstRequest)
	if firstResponse.Code != http.StatusOK {
		t.Fatalf("first list status = %d, body = %s", firstResponse.Code, firstResponse.Body.String())
	}
	if firstResponse.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("first list Cache-Control = %q", firstResponse.Header().Get("Cache-Control"))
	}
	var firstPage ProductPage
	if err := json.Unmarshal(firstResponse.Body.Bytes(), &firstPage); err != nil {
		t.Fatalf("decode first page: %v", err)
	}
	if len(firstPage.Items) != 1 || firstPage.Items[0].Slug != "edgecli" || firstPage.NextCursor == nil {
		t.Fatalf("first page = %+v", firstPage)
	}
	if len(products.listRequests) != 1 || products.listRequests[0].Limit != 1 ||
		products.listActors[0].TokenID != authenticator.actor.TokenID {
		t.Fatalf("first list actor/request = %+v, %+v", products.listActors, products.listRequests)
	}

	secondRequest := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/products?limit=2&cursor="+string(*firstPage.NextCursor),
		nil,
	)
	secondRequest.Header.Set("Authorization", "Bearer ar1.valid")
	secondResponse := httptest.NewRecorder()
	handler.ServeHTTP(secondResponse, secondRequest)
	if secondResponse.Code != http.StatusOK {
		t.Fatalf("second list status = %d, body = %s", secondResponse.Code, secondResponse.Body.String())
	}
	if len(products.listRequests) != 2 {
		t.Fatalf("List() calls = %d, want 2", len(products.listRequests))
	}
	second := products.listRequests[1]
	if second.Limit != 2 || second.After == nil ||
		second.After.Slug != "edgecli" ||
		second.After.ID != nextID ||
		!second.After.CreatedAt.Equal(createdAt) {
		t.Fatalf("decoded second request = %+v", second)
	}

	badCursorRequest := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/products?cursor="+string(*encodeCursor(cursorEnvelope{
			Kind: "repositories", Key: "edgecli", ID: nextID, CreatedAt: createdAt,
		})),
		nil,
	)
	badCursorRequest.Header.Set("Authorization", "Bearer ar1.valid")
	badCursorResponse := httptest.NewRecorder()
	handler.ServeHTTP(badCursorResponse, badCursorRequest)
	if badCursorResponse.Code != http.StatusBadRequest {
		t.Fatalf("bad cursor status = %d, body = %s", badCursorResponse.Code, badCursorResponse.Body.String())
	}
	if len(products.listRequests) != 2 {
		t.Fatalf("List() calls after bad cursor = %d, want 2", len(products.listRequests))
	}
}

func TestProductRoutesRequireBearerAuthentication(t *testing.T) {
	authenticator := &identityServiceStub{actor: adminAPIActor()}
	products := &productServiceStub{}
	handler := NewServer(Dependencies{
		Readiness: &readinessProbe{}, Authenticator: authenticator, Products: products,
	})

	request := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", response.Code, response.Body.String())
	}
	if products.totalCalls() != 0 {
		t.Fatalf("product service calls = %d, want 0", products.totalCalls())
	}
}

func assertProductDTO(
	t *testing.T,
	response *httptest.ResponseRecorder,
	want productdomain.Product,
	wantInstallKey uuid.UUID,
) {
	t.Helper()
	if response.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("Cache-Control = %q", response.Header().Get("Cache-Control"))
	}
	var got Product
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode product: %v", err)
	}
	if got.Id != want.ID ||
		got.Slug != want.Slug ||
		got.DisplayName != want.DisplayName ||
		got.Description != want.Description ||
		got.CommandName != want.CommandName ||
		got.Repository != want.RepositoryKey ||
		got.Package != want.PackageName ||
		got.InstallKey != wantInstallKey ||
		!got.CreatedAt.Equal(want.CreatedAt) ||
		!got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Fatalf("product DTO = %+v, want domain product %+v", got, want)
	}
	if want.CurrentStableVersion != nil &&
		(got.CurrentVersion == nil || *got.CurrentVersion != *want.CurrentStableVersion) {
		t.Fatalf("currentVersion = %v, want %v", got.CurrentVersion, want.CurrentStableVersion)
	}
	if want.CurrentStablePublishedAt != nil &&
		(got.PublishedAt == nil || !got.PublishedAt.Equal(*want.CurrentStablePublishedAt)) {
		t.Fatalf("publishedAt = %v, want %v", got.PublishedAt, want.CurrentStablePublishedAt)
	}
	if len(got.Platforms) != len(want.Platforms) {
		t.Fatalf("platforms = %+v, want %+v", got.Platforms, want.Platforms)
	}
	if len(want.Platforms) > 0 {
		platform := got.Platforms[0]
		if platform.Os != want.Platforms[0].OS ||
			platform.Arch != want.Platforms[0].Arch ||
			platform.Variant != want.Platforms[0].Variant ||
			string(platform.Strategy) != want.Platforms[0].Strategy ||
			string(platform.Format) != want.Platforms[0].Format {
			t.Fatalf("platform DTO = %+v, want %+v", platform, want.Platforms[0])
		}
	}
}

type productServiceStub struct {
	createRequest productdomain.CreateRequest
	createResult  productdomain.Product
	createError   error
	createCalls   int

	getActor  identity.Actor
	getSlug   string
	getResult productdomain.Product
	getError  error
	getCalls  int

	listActors   []identity.Actor
	listRequests []productdomain.ListRequest
	listResults  []productdomain.Page
	listError    error

	rotateRequest productdomain.RotateInstallKeyRequest
	rotateResult  productdomain.Product
	rotateError   error
	rotateCalls   int

	installKey       uuid.UUID
	installKeyResult productdomain.Product
	installKeyError  error
	installKeyCalls  int
}

func (s *productServiceStub) Create(_ context.Context, request productdomain.CreateRequest) (productdomain.Product, error) {
	s.createCalls++
	s.createRequest = request
	return s.createResult, s.createError
}

func (s *productServiceStub) Get(_ context.Context, actor identity.Actor, slug string) (productdomain.Product, error) {
	s.getCalls++
	s.getActor = actor
	s.getSlug = slug
	return s.getResult, s.getError
}

func (s *productServiceStub) List(_ context.Context, actor identity.Actor, request productdomain.ListRequest) (productdomain.Page, error) {
	s.listActors = append(s.listActors, actor)
	s.listRequests = append(s.listRequests, request)
	if s.listError != nil {
		return productdomain.Page{}, s.listError
	}
	if len(s.listResults) == 0 {
		return productdomain.Page{}, nil
	}
	result := s.listResults[0]
	s.listResults = s.listResults[1:]
	return result, nil
}

func (s *productServiceStub) RotateInstallKey(_ context.Context, request productdomain.RotateInstallKeyRequest) (productdomain.Product, error) {
	s.rotateCalls++
	s.rotateRequest = request
	return s.rotateResult, s.rotateError
}

func (s *productServiceStub) GetByInstallKey(_ context.Context, key uuid.UUID) (productdomain.Product, error) {
	s.installKeyCalls++
	s.installKey = key
	return s.installKeyResult, s.installKeyError
}

func (s *productServiceStub) totalCalls() int {
	return s.createCalls + s.getCalls + len(s.listRequests) + s.rotateCalls + s.installKeyCalls
}
