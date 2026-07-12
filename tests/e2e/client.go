package e2e

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type apiClient struct {
	baseURL *url.URL
	http    *http.Client
}

type apiResponse struct {
	Status int
	Header http.Header
	Body   []byte
}

func newAPIClient(rawBaseURL string) (*apiClient, error) {
	baseURL, err := url.Parse(strings.TrimRight(rawBaseURL, "/") + "/")
	if err != nil {
		return nil, fmt.Errorf("parse E2E base URL: %w", err)
	}
	if (baseURL.Scheme != "http" && baseURL.Scheme != "https") || baseURL.Host == "" {
		return nil, fmt.Errorf("E2E base URL must be an absolute HTTP URL")
	}
	return &apiClient{
		baseURL: baseURL,
		http: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (c *apiClient) do(
	ctx context.Context,
	method string,
	path string,
	token string,
	body []byte,
	headers map[string]string,
) (apiResponse, error) {
	reference, err := url.Parse(strings.TrimPrefix(path, "/"))
	if err != nil {
		return apiResponse{}, fmt.Errorf("parse request path: %w", err)
	}
	requestURL := c.baseURL.ResolveReference(reference)
	request, err := http.NewRequestWithContext(ctx, method, requestURL.String(), bytes.NewReader(body))
	if err != nil {
		return apiResponse{}, fmt.Errorf("create request: %w", err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return apiResponse{}, fmt.Errorf("%s %s: %w", method, requestURL.Redacted(), err)
	}
	defer func() { _ = response.Body.Close() }()
	content, err := io.ReadAll(response.Body)
	if err != nil {
		return apiResponse{}, fmt.Errorf("read %s %s response: %w", method, requestURL.Redacted(), err)
	}
	return apiResponse{Status: response.StatusCode, Header: response.Header.Clone(), Body: content}, nil
}

func composeOutput(ctx context.Context, project string, arguments ...string) ([]byte, error) {
	root, err := repositoryRoot()
	if err != nil {
		return nil, err
	}
	composeArguments := []string{"compose", "-f", filepath.Join(root, "compose.yaml"), "-p", project}
	composeArguments = append(composeArguments, arguments...)
	command := exec.CommandContext(ctx, "docker", composeArguments...)
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("docker compose %s: %w: %s", strings.Join(arguments, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func bootstrapAdmin(ctx context.Context, project, name string) (string, error) {
	if configured := strings.TrimSpace(os.Getenv("E2E_ADMIN_TOKEN")); configured != "" {
		return configured, nil
	}
	output, err := composeOutput(
		ctx,
		project,
		"exec", "-T", "api", "/app/artifact-repository", "bootstrap-admin", "--name", name,
	)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(output))
	if token == "" {
		return "", fmt.Errorf("bootstrap-admin returned an empty token")
	}
	return token, nil
}

func trustedPublicKey(ctx context.Context, project string) (ed25519.PublicKey, error) {
	var (
		encoded []byte
		err     error
	)
	if path := strings.TrimSpace(os.Getenv("E2E_PUBLIC_KEY_FILE")); path != "" {
		encoded, err = os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read E2E public key file: %w", err)
		}
	} else {
		encoded, err = composeOutput(ctx, project, "exec", "-T", "api", "cat", "/app/keys/public.pem")
		if err != nil {
			return nil, err
		}
	}
	block, trailing := pem.Decode(encoded)
	if block == nil || len(bytes.TrimSpace(trailing)) != 0 {
		return nil, fmt.Errorf("trusted public key is not exactly one PEM block")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse trusted public key: %w", err)
	}
	publicKey, ok := parsed.(ed25519.PublicKey)
	if !ok || len(publicKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("trusted public key is not Ed25519")
	}
	return append(ed25519.PublicKey(nil), publicKey...), nil
}

func waitForReady(ctx context.Context, rawBaseURL string, timeout time.Duration) error {
	client, err := newAPIClient(rawBaseURL)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	var last string
	for {
		response, requestErr := client.do(ctx, http.MethodGet, "/readyz", "", nil, nil)
		if requestErr == nil && response.Status == http.StatusOK {
			return nil
		}
		if requestErr != nil {
			last = requestErr.Error()
		} else {
			last = fmt.Sprintf("status %d: %s", response.Status, strings.TrimSpace(string(response.Body)))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("wait for %s readiness: %s", rawBaseURL, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func downloadWithoutCredentials(ctx context.Context, rawURL string) (apiResponse, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return apiResponse{}, fmt.Errorf("create anonymous download request: %w", err)
	}
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	if err != nil {
		return apiResponse{}, fmt.Errorf("anonymous download from %s: %w", request.URL.Redacted(), err)
	}
	defer func() { _ = response.Body.Close() }()
	content, err := io.ReadAll(response.Body)
	if err != nil {
		return apiResponse{}, fmt.Errorf("read anonymous download: %w", err)
	}
	return apiResponse{Status: response.StatusCode, Header: response.Header.Clone(), Body: content}, nil
}

func repositoryRoot() (string, error) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("locate E2E client source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "compose.yaml")); err != nil {
		return "", fmt.Errorf("locate repository root: %w", err)
	}
	return root, nil
}
