package secrets

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Resolver returns Secret Manager payload values. The controller injects
// them as Pod env; values must not be written to SQL or logs.
type Resolver interface {
	Resolve(ctx context.Context, secretID, version string) (string, error)
}

// Map is an in-memory resolver for tests.
type Map map[string]string

func (m Map) Resolve(_ context.Context, secretID, version string) (string, error) {
	if version == "" {
		version = "latest"
	}
	key := secretID + "@" + version
	if v, ok := m[key]; ok {
		return v, nil
	}
	if v, ok := m[secretID]; ok {
		return v, nil
	}
	return "", fmt.Errorf("secret %s@%s not found", secretID, version)
}

const (
	metadataTokenURL = "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token"
	secretAPIHost    = "https://secretmanager.googleapis.com"
)

// GCP resolves secrets via the Secret Manager REST API and GCE metadata.
type GCP struct {
	Project     string
	Client      *http.Client
	MetadataURL string
	APIURL      string
	Token       string
}

func (g *GCP) Resolve(ctx context.Context, secretID, version string) (string, error) {
	if g == nil || strings.TrimSpace(g.Project) == "" {
		return "", fmt.Errorf("secret manager project is not configured")
	}
	if version == "" {
		version = "latest"
	}
	token, err := g.accessToken(ctx)
	if err != nil {
		return "", err
	}
	api := g.APIURL
	if api == "" {
		api = secretAPIHost
	}
	url := fmt.Sprintf("%s/v1/projects/%s/secrets/%s/versions/%s:access",
		strings.TrimRight(api, "/"), g.Project, secretID, version)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := g.client()
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("secret manager status %d", resp.StatusCode)
	}
	var out struct {
		Payload struct {
			Data string `json:"data"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(out.Payload.Data)
	if err != nil {
		return "", fmt.Errorf("secret payload: %w", err)
	}
	return string(raw), nil
}

func (g *GCP) accessToken(ctx context.Context) (string, error) {
	if g.Token != "" {
		return g.Token, nil
	}
	meta := g.MetadataURL
	if meta == "" {
		meta = metadataTokenURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, meta, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := g.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("metadata token status %d", resp.StatusCode)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&tok); err != nil {
		return "", err
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("metadata token empty")
	}
	return tok.AccessToken, nil
}

func (g *GCP) client() *http.Client {
	if g.Client != nil {
		return g.Client
	}
	return &http.Client{Timeout: 10 * time.Second}
}
