package cli

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	api "superfan.myasustor.com/fanchao/artifact-repository/internal/api"
)

const maxErrorBody = 1 << 20

type client struct {
	baseURL *url.URL
	token   string
	http    *http.Client
}

type apiError struct {
	Status     int
	Code       string
	Detail     string
	RequestID  string
	RetryAfter string
}

func (e apiError) Error() string {
	message := fmt.Sprintf("API returned HTTP %d", e.Status)
	if e.Code != "" {
		message += " (" + e.Code + ")"
	}
	if e.Detail != "" {
		message += ": " + e.Detail
	}
	if e.RequestID != "" {
		message += " [request-id: " + e.RequestID + "]"
	}
	if e.RetryAfter != "" {
		message += " [retry-after: " + e.RetryAfter + "]"
	}
	return message
}

func newClient(rawBaseURL, token string) (*client, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawBaseURL))
	if err != nil {
		return nil, fmt.Errorf("parse API URL: %w", err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("API URL must be an absolute HTTP or HTTPS URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("API URL must not contain user info, query parameters, or a fragment")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return nil, fmt.Errorf("API URL must not contain a path")
	}
	parsed.Path = ""
	parsed.RawPath = ""
	return &client{
		baseURL: parsed,
		token:   token,
		http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}, nil
}

func (c *client) upload(ctx context.Context, localPath string, ref artifactReference, mediaType, properties string) (api.Artifact, error) {
	file, err := os.Open(localPath)
	if err != nil {
		return api.Artifact{}, fmt.Errorf("open upload file: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return api.Artifact{}, fmt.Errorf("stat upload file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return api.Artifact{}, fmt.Errorf("upload source must be a regular file")
	}
	hash := sha256.New()
	count, err := io.Copy(hash, file)
	if err != nil {
		return api.Artifact{}, fmt.Errorf("hash upload file: %w", err)
	}
	if count != info.Size() {
		return api.Artifact{}, fmt.Errorf("upload file changed while hashing")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return api.Artifact{}, fmt.Errorf("rewind upload file: %w", err)
	}
	checksum := hex.EncodeToString(hash.Sum(nil))
	request, err := c.apiRequest(ctx, http.MethodPut, artifactAPIPath(ref), file)
	if err != nil {
		return api.Artifact{}, err
	}
	request.ContentLength = info.Size()
	request.Header.Set("Content-Type", mediaType)
	request.Header.Set("X-Checksum-Sha256", checksum)
	if properties != "" {
		request.Header.Set("X-Artifact-Properties", properties)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return api.Artifact{}, fmt.Errorf("upload artifact: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusCreated {
		return api.Artifact{}, decodeAPIError(response)
	}
	var artifact api.Artifact
	if err := decodeJSONBody(response.Body, &artifact); err != nil {
		return api.Artifact{}, fmt.Errorf("decode upload response: %w", err)
	}
	if artifact.Sha256 != checksum || artifact.Size != info.Size() || artifact.Path != ref.Path || artifact.Repository != ref.Repository {
		return api.Artifact{}, fmt.Errorf("upload response metadata does not match the uploaded artifact")
	}
	return artifact, nil
}

func (c *client) metadata(ctx context.Context, ref artifactReference) (api.Artifact, error) {
	request, err := c.apiRequest(ctx, http.MethodGet, metadataAPIPath(ref), nil)
	if err != nil {
		return api.Artifact{}, err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return api.Artifact{}, fmt.Errorf("get artifact metadata: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return api.Artifact{}, decodeAPIError(response)
	}
	var artifact api.Artifact
	if err := decodeJSONBody(response.Body, &artifact); err != nil {
		return api.Artifact{}, fmt.Errorf("decode artifact metadata: %w", err)
	}
	if err := validateArtifactMetadata(artifact, ref); err != nil {
		return api.Artifact{}, err
	}
	return artifact, nil
}

func (c *client) downloadArtifact(ctx context.Context, ref artifactReference, redirect bool) (*http.Response, error) {
	query := url.Values{"redirect": []string{strconv.FormatBool(redirect)}}
	request, err := c.apiRequest(ctx, http.MethodGet, artifactAPIPath(ref)+"?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("download artifact: %w", err)
	}
	if response.StatusCode == http.StatusTemporaryRedirect && redirect {
		location := response.Header.Get("Location")
		_ = response.Body.Close()
		return c.openAnonymous(ctx, request.URL, location)
	}
	if response.StatusCode != http.StatusOK {
		defer func() { _ = response.Body.Close() }()
		return nil, decodeAPIError(response)
	}
	return response, nil
}

func (c *client) resolve(ctx context.Context, ref packageReference, selection channelSelection) (api.ResolveResponse, error) {
	query := url.Values{
		"arch":     []string{selection.Arch},
		"os":       []string{selection.OS},
		"redirect": []string{strconv.FormatBool(selection.Redirect)},
	}
	if selection.Variant != "" {
		query.Set("variant", selection.Variant)
	}
	if selection.Role != "" {
		query.Set("role", selection.Role)
	}
	path := "/api/v1/repositories/" + ref.Repository + "/packages/" + ref.Package + "/channels/" + selection.Channel + "/resolve?" + query.Encode()
	request, err := c.apiRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.ResolveResponse{}, err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return api.ResolveResponse{}, fmt.Errorf("resolve Channel artifact: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return api.ResolveResponse{}, decodeAPIError(response)
	}
	var resolved api.ResolveResponse
	if err := decodeJSONBody(response.Body, &resolved); err != nil {
		return api.ResolveResponse{}, fmt.Errorf("decode Channel resolution: %w", err)
	}
	return resolved, nil
}

func (c *client) downloadResolved(ctx context.Context, rawReference string) (*http.Response, error) {
	reference, err := url.Parse(rawReference)
	if err != nil {
		return nil, fmt.Errorf("parse resolved download URL: %w", err)
	}
	if reference.User != nil || (reference.Scheme != "" && reference.Scheme != "http" && reference.Scheme != "https") {
		return nil, fmt.Errorf("resolved download URL is not an HTTP URL")
	}
	if reference.IsAbs() {
		return c.openAnonymous(ctx, nil, reference.String())
	}
	if reference.Host != "" || !strings.HasPrefix(reference.Path, "/") {
		return nil, fmt.Errorf("relative resolved download URL must be an absolute API path")
	}
	resolved := c.baseURL.ResolveReference(reference)
	request, err := c.apiRequestURL(ctx, http.MethodGet, resolved, nil)
	if err != nil {
		return nil, err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("download resolved artifact: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		defer func() { _ = response.Body.Close() }()
		return nil, decodeAPIError(response)
	}
	return response, nil
}

func (c *client) openAnonymous(ctx context.Context, origin *url.URL, rawLocation string) (*http.Response, error) {
	location, err := url.Parse(rawLocation)
	if err != nil {
		return nil, fmt.Errorf("parse redirect download URL: %w", err)
	}
	if origin != nil {
		location = origin.ResolveReference(location)
	}
	if (location.Scheme != "http" && location.Scheme != "https") || location.Host == "" || location.User != nil {
		return nil, fmt.Errorf("redirect download URL is not an absolute HTTP URL")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, location.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create anonymous download request: %w", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("download from presigned URL: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		defer func() { _ = response.Body.Close() }()
		return nil, fmt.Errorf("presigned download returned HTTP %d", response.StatusCode)
	}
	return response, nil
}

func (c *client) apiRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	reference, err := url.Parse(path)
	if err != nil {
		return nil, fmt.Errorf("build API request path: %w", err)
	}
	return c.apiRequestURL(ctx, method, c.baseURL.ResolveReference(reference), body)
}

func (c *client) apiRequestURL(ctx context.Context, method string, target *url.URL, body io.Reader) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, fmt.Errorf("create API request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "application/json")
	return request, nil
}

func artifactAPIPath(ref artifactReference) string {
	return "/api/v1/repositories/" + ref.Repository + "/artifacts/" + ref.Path
}

func metadataAPIPath(ref artifactReference) string {
	return "/api/v1/repositories/" + ref.Repository + "/metadata/" + ref.Path
}

func validateArtifactMetadata(artifact api.Artifact, ref artifactReference) error {
	if artifact.Repository != ref.Repository || artifact.Path != ref.Path {
		return fmt.Errorf("artifact metadata does not match the requested path")
	}
	if len(artifact.Sha256) != sha256.Size*2 {
		return fmt.Errorf("artifact metadata contains an invalid SHA-256")
	}
	if _, err := hex.DecodeString(artifact.Sha256); err != nil {
		return fmt.Errorf("artifact metadata contains an invalid SHA-256")
	}
	if artifact.Size < 0 {
		return fmt.Errorf("artifact metadata contains a negative size")
	}
	return nil
}

func decodeAPIError(response *http.Response) error {
	body, err := io.ReadAll(io.LimitReader(response.Body, maxErrorBody+1))
	if err != nil {
		return fmt.Errorf("API returned HTTP %d and its error body could not be read: %w", response.StatusCode, err)
	}
	result := apiError{Status: response.StatusCode, RetryAfter: response.Header.Get("Retry-After")}
	if len(body) <= maxErrorBody {
		var problem api.Problem
		if json.Unmarshal(body, &problem) == nil {
			result.Code = problem.Code
			result.RequestID = problem.RequestId
			if problem.Detail != nil {
				result.Detail = *problem.Detail
			} else {
				result.Detail = problem.Title
			}
		}
	}
	return result
}

func decodeJSONBody(body io.Reader, destination any) error {
	decoder := json.NewDecoder(io.LimitReader(body, maxErrorBody+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("response contains multiple JSON values")
		}
		return err
	}
	return nil
}

func newJSONEncoder(destination io.Writer) *json.Encoder {
	encoder := json.NewEncoder(destination)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder
}

func decodeBase64URL(value string) ([]byte, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, err
	}
	return decoded, nil
}
