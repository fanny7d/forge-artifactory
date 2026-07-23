package forgeupdate

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

const DefaultMaxResolveBytes int64 = 2 << 20

var (
	sourceCoordinate         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
	sourceOptionalCoordinate = regexp.MustCompile(`^[A-Za-z0-9._+-]{0,64}$`)
)

// HTTPSourceConfig identifies one private Forge product install endpoint.
// InstallKey is sent only as a URL path segment; HTTPSource never adds bearer
// authentication or cookies.
type HTTPSourceConfig struct {
	BaseURL    string
	Product    string
	InstallKey string
	HTTPClient *http.Client
}

// HTTPSource resolves signed releases and opens their artifact streams from
// Forge's product-scoped /i endpoints.
type HTTPSource struct {
	baseURL    *url.URL
	product    string
	installKey string
	client     *http.Client
}

// HTTPError reports an HTTP status without including the install-key URL.
type HTTPError struct {
	StatusCode int
	RetryAfter string
}

func (err *HTTPError) Error() string {
	if err == nil {
		return ErrHTTPStatus.Error()
	}
	status := http.StatusText(err.StatusCode)
	if status == "" {
		return fmt.Sprintf("%s: %d", ErrHTTPStatus, err.StatusCode)
	}
	return fmt.Sprintf("%s: %d %s", ErrHTTPStatus, err.StatusCode, status)
}

func (err *HTTPError) Is(target error) bool {
	return target == ErrHTTPStatus
}

type sourceRequestError struct {
	operation string
	cause     error
}

func (err *sourceRequestError) Error() string {
	return fmt.Sprintf("%s: %s", ErrSource, err.operation)
}

func (err *sourceRequestError) Unwrap() error {
	return err.cause
}

func (err *sourceRequestError) Is(target error) bool {
	return target == ErrSource
}

// ResolvedArtifact is unsigned transport metadata returned by Forge. Client
// binds every field to the selected artifact in the signed manifest before it
// creates a Plan.
type ResolvedArtifact struct {
	Path    string
	OS      string
	Arch    string
	Variant string
	Role    string
	SHA256  string
	Size    int64
}

// SourceResolution is an opaque response from HTTPSource.Resolve. Its download
// reference is pinned to the resolved signed version and artifact digest. It
// can be inspected through getters and opened only by the source that created
// it.
type SourceResolution struct {
	owner       *HTTPSource
	signed      SignedManifest
	version     string
	artifact    ResolvedArtifact
	downloadURL url.URL
}

func (resolution SourceResolution) SignedManifest() SignedManifest {
	return cloneSignedManifest(resolution.signed)
}

func (resolution SourceResolution) Version() string {
	return resolution.version
}

func (resolution SourceResolution) Artifact() ResolvedArtifact {
	return resolution.artifact
}

type resolveDocument struct {
	Version   string `json:"version"`
	Manifest  string `json:"manifest"`
	KeyID     string `json:"keyId"`
	Signature string `json:"signature"`
	Artifact  struct {
		Path    string `json:"path"`
		OS      string `json:"os"`
		Arch    string `json:"arch"`
		Variant string `json:"variant"`
		Role    string `json:"role"`
		SHA256  string `json:"sha256"`
		Size    int64  `json:"size"`
	} `json:"artifact"`
	DownloadURL string `json:"downloadUrl"`
}

// NewHTTPSource validates and freezes transport configuration. BaseURL is the
// Forge origin (for example, https://forge.example) and must not contain a
// path, credentials, query, or fragment.
func NewHTTPSource(config HTTPSourceConfig) (*HTTPSource, error) {
	baseURL, err := url.Parse(config.BaseURL)
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" ||
		(baseURL.Scheme != "http" && baseURL.Scheme != "https") ||
		baseURL.User != nil || baseURL.Opaque != "" ||
		(baseURL.Path != "" && baseURL.Path != "/") ||
		baseURL.RawPath != "" || baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return nil, fmt.Errorf("forgeupdate: HTTP source BaseURL must be an absolute HTTP(S) origin")
	}
	if !validProductSlug.MatchString(config.Product) {
		return nil, fmt.Errorf("forgeupdate: HTTP source Product is invalid")
	}
	installKey, err := uuid.Parse(config.InstallKey)
	if err != nil || installKey == uuid.Nil {
		return nil, fmt.Errorf("forgeupdate: HTTP source InstallKey is invalid")
	}
	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	client := *httpClient
	client.Jar = nil
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	origin := &url.URL{
		Scheme: strings.ToLower(baseURL.Scheme),
		Host:   baseURL.Host,
	}
	return &HTTPSource{
		baseURL: origin, product: config.Product, installKey: installKey.String(), client: &client,
	}, nil
}

func (source *HTTPSource) Product() string {
	if source == nil {
		return ""
	}
	return source.product
}

// Resolve fetches the exact signed stable manifest for Selection's platform.
// It does not verify the signature and does not download artifact bytes.
func (source *HTTPSource) Resolve(ctx context.Context, selection Selection) (SourceResolution, error) {
	if source == nil || source.baseURL == nil || source.client == nil {
		return SourceResolution{}, fmt.Errorf("%w: HTTP source is nil", ErrSource)
	}
	if selection.Package != source.product {
		return SourceResolution{}, fmt.Errorf("%w: source product does not match selection package", ErrInvalidResponse)
	}
	if !sourceCoordinate.MatchString(selection.OS) || !sourceCoordinate.MatchString(selection.Arch) ||
		!sourceOptionalCoordinate.MatchString(selection.Variant) || selection.Role != "binary" {
		return SourceResolution{}, fmt.Errorf("%w: invalid source selection", ErrInvalidResponse)
	}
	query := url.Values{"os": {selection.OS}, "arch": {selection.Arch}}
	if selection.Variant != "" {
		query.Set("variant", selection.Variant)
	}
	endpoint := source.endpoint("resolve", query)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return SourceResolution{}, fmt.Errorf("%w: create resolve request", ErrSource)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Accept-Encoding", "identity")
	response, err := source.client.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return SourceResolution{}, ctxErr
		}
		return SourceResolution{}, &sourceRequestError{operation: "resolve request failed", cause: err}
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		drainResponse(response.Body)
		return SourceResolution{}, httpStatusError(response)
	}
	if encoding := response.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return SourceResolution{}, fmt.Errorf("%w: resolve response is content-encoded", ErrInvalidResponse)
	}
	encoded, err := readBounded(response.Body, DefaultMaxResolveBytes)
	if err != nil {
		return SourceResolution{}, err
	}
	if err := rejectDuplicateJSONFields(encoded); err != nil {
		return SourceResolution{}, fmt.Errorf("%w: %v", ErrInvalidResponse, err)
	}
	var document resolveDocument
	if err := decodeStrictJSON(encoded, &document, true); err != nil {
		return SourceResolution{}, fmt.Errorf("%w: decode resolve response: %v", ErrInvalidResponse, err)
	}
	manifest, err := base64.RawURLEncoding.Strict().DecodeString(document.Manifest)
	if err != nil || len(manifest) == 0 {
		return SourceResolution{}, fmt.Errorf("%w: manifest is not strict raw base64url", ErrInvalidResponse)
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(document.Signature)
	if err != nil || len(signature) == 0 {
		return SourceResolution{}, fmt.Errorf("%w: signature is not strict raw base64url", ErrInvalidResponse)
	}
	artifact := ResolvedArtifact{
		Path: document.Artifact.Path, OS: document.Artifact.OS, Arch: document.Artifact.Arch,
		Variant: document.Artifact.Variant, Role: document.Artifact.Role,
		SHA256: document.Artifact.SHA256, Size: document.Artifact.Size,
	}
	if document.Version == "" || document.KeyID == "" || artifact.Path == "" ||
		artifact.OS == "" || artifact.Arch == "" || artifact.Role == "" ||
		artifact.Size < 0 || !validSHA256(artifact.SHA256) {
		return SourceResolution{}, fmt.Errorf("%w: required resolve field is invalid", ErrInvalidResponse)
	}
	downloadQuery := cloneQuery(query)
	downloadQuery.Set("version", document.Version)
	downloadQuery.Set("sha256", artifact.SHA256)
	downloadURL, err := source.validateDownloadReference(document.DownloadURL, downloadQuery)
	if err != nil {
		return SourceResolution{}, err
	}
	return SourceResolution{
		owner: source,
		signed: SignedManifest{
			KeyID: document.KeyID, Manifest: manifest, Signature: signature,
		},
		version: document.Version, artifact: artifact, downloadURL: *downloadURL,
	}, nil
}

// Open opens the selected artifact only after binding the resolution and
// download response headers to a VerifiedRelease.
func (source *HTTPSource) Open(
	ctx context.Context,
	resolution SourceResolution,
	release VerifiedRelease,
) (io.ReadCloser, error) {
	if source == nil || resolution.owner != source {
		return nil, fmt.Errorf("%w: resolution belongs to another source", ErrInvalidResponse)
	}
	if err := bindResolution(resolution, release); err != nil {
		return nil, err
	}
	artifact := release.artifact
	if release.manifest.SchemaVersion != 2 || artifact.Strategy == "" || artifact.Format == "" {
		return nil, fmt.Errorf("%w: Forge install downloads require manifest v2", ErrInvalidManifest)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, resolution.downloadURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: create download request", ErrSource)
	}
	request.Header.Set("Accept", "application/octet-stream")
	request.Header.Set("Accept-Encoding", "identity")
	response, err := source.client.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, &sourceRequestError{operation: "download request failed", cause: err}
	}
	if response.StatusCode != http.StatusOK {
		defer func() { _ = response.Body.Close() }()
		drainResponse(response.Body)
		return nil, httpStatusError(response)
	}
	closeWithError := true
	defer func() {
		if closeWithError {
			_ = response.Body.Close()
		}
	}()
	if encoding := response.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return nil, fmt.Errorf("%w: artifact response is content-encoded", ErrInvalidResponse)
	}
	if response.Header.Get("X-Checksum-Sha256") != artifact.SHA256 ||
		response.Header.Get("X-Forge-Version") != release.manifest.Version ||
		response.Header.Get("X-Forge-Install-Strategy") != string(artifact.Strategy) ||
		response.Header.Get("X-Forge-Install-Format") != string(artifact.Format) {
		return nil, fmt.Errorf("%w: artifact headers do not match signed manifest", ErrInvalidResponse)
	}
	if response.ContentLength >= 0 && response.ContentLength != artifact.Size {
		return nil, fmt.Errorf("%w: artifact Content-Length does not match signed manifest", ErrInvalidResponse)
	}
	closeWithError = false
	return response.Body, nil
}

func (source *HTTPSource) endpoint(action string, query url.Values) *url.URL {
	endpoint := *source.baseURL
	endpoint.Path = "/i/" + source.installKey + "/" + source.product + "/" + action
	endpoint.RawQuery = query.Encode()
	return &endpoint
}

func (source *HTTPSource) validateDownloadReference(raw string, expectedQuery url.Values) (*url.URL, error) {
	if raw == "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return nil, fmt.Errorf("%w: downloadUrl must be an origin-relative path", ErrInvalidResponse)
	}
	reference, err := url.Parse(raw)
	if err != nil || reference.IsAbs() || reference.Host != "" || reference.User != nil ||
		reference.Opaque != "" || reference.Fragment != "" || reference.RawFragment != "" ||
		reference.RawPath != "" {
		return nil, fmt.Errorf("%w: downloadUrl is not a safe origin-relative URL", ErrInvalidResponse)
	}
	expected := source.endpoint("download", expectedQuery)
	if reference.Path != expected.Path {
		return nil, fmt.Errorf("%w: downloadUrl path does not match product", ErrInvalidResponse)
	}
	query, err := url.ParseQuery(reference.RawQuery)
	if err != nil || !equalQuery(query, expectedQuery) {
		return nil, fmt.Errorf("%w: downloadUrl query does not match selection", ErrInvalidResponse)
	}
	return expected, nil
}

func bindResolution(resolution SourceResolution, release VerifiedRelease) error {
	manifest := release.manifest
	artifact := release.artifact
	resolved := resolution.artifact
	if resolution.version != manifest.Version ||
		resolved.Path != artifact.Path ||
		resolved.OS != artifact.OS ||
		resolved.Arch != artifact.Arch ||
		resolved.Variant != artifact.Variant ||
		resolved.Role != artifact.Role ||
		resolved.SHA256 != artifact.SHA256 ||
		resolved.Size != artifact.Size {
		return fmt.Errorf("%w: resolve metadata does not match signed manifest", ErrInvalidResponse)
	}
	return nil
}

func cloneSignedManifest(source SignedManifest) SignedManifest {
	return SignedManifest{
		KeyID:     source.KeyID,
		Manifest:  append([]byte(nil), source.Manifest...),
		Signature: append([]byte(nil), source.Signature...),
	}
}

func validSHA256(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func equalQuery(left, right url.Values) bool {
	if len(left) != len(right) {
		return false
	}
	for key, expected := range right {
		actual, ok := left[key]
		if !ok || len(actual) != len(expected) {
			return false
		}
		for index := range expected {
			if actual[index] != expected[index] {
				return false
			}
		}
	}
	return true
}

func cloneQuery(source url.Values) url.Values {
	clone := make(url.Values, len(source))
	for key, values := range source {
		clone[key] = append([]string(nil), values...)
	}
	return clone
}

func readBounded(source io.Reader, maximum int64) ([]byte, error) {
	limited := &io.LimitedReader{R: source, N: maximum + 1}
	encoded, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("%w: read response body", ErrSource)
	}
	if int64(len(encoded)) > maximum {
		return nil, fmt.Errorf("%w: response exceeds %d bytes", ErrInvalidResponse, maximum)
	}
	return encoded, nil
}

func drainResponse(source io.Reader) {
	_, _ = io.CopyN(io.Discard, source, 64<<10)
}

func httpStatusError(response *http.Response) error {
	return &HTTPError{
		StatusCode: response.StatusCode,
		RetryAfter: response.Header.Get("Retry-After"),
	}
}
