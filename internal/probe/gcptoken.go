package probe

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// metadataIdentityURL mints a Google-signed OIDC ID token for the attached
// service account. Mirrors the GCE metadata usage in internal/secrets.
// A var, not a const, so tests can point it at a stub server.
var metadataIdentityURL = "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/identity"

// MetadataIDToken fetches a fresh ID token whose `aud` claim is audience. It
// only works on GCP (GKE with Workload Identity, GCE, Cloud Run, ...).
func MetadataIDToken(ctx context.Context, hc *http.Client, audience string) (string, error) {
	if audience == "" {
		return "", fmt.Errorf("id token audience is required")
	}
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataIdentityURL, nil)
	if err != nil {
		return "", err
	}
	q := req.URL.Query()
	q.Set("audience", audience)
	q.Set("format", "full")
	req.URL.RawQuery = q.Encode()
	req.Header.Set("Metadata-Flavor", "Google")

	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("metadata identity: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("metadata identity status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return strings.TrimSpace(string(body)), nil
}

// IDTokenSource returns a token callback for Client.WithTokenFunc that caches a
// metadata ID token and refetches it once it is within 5 minutes of expiry.
func IDTokenSource(hc *http.Client, audience string) func(context.Context) (string, error) {
	var (
		mu     sync.Mutex
		cached string
		exp    time.Time
	)
	return func(ctx context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if cached != "" && time.Until(exp) > 5*time.Minute {
			return cached, nil
		}
		tok, err := MetadataIDToken(ctx, hc, audience)
		if err != nil {
			return "", err
		}
		cached = tok
		exp = jwtExpiry(tok)
		return tok, nil
	}
}

// jwtExpiry reads the `exp` claim without verifying the signature. A missing or
// unparseable exp yields a conservative 10-minute lifetime.
func jwtExpiry(token string) time.Time {
	fallback := time.Now().Add(10 * time.Minute)
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return fallback
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return fallback
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp == 0 {
		return fallback
	}
	return time.Unix(claims.Exp, 0)
}

// jwtClaims decodes a JWT payload (no signature check) for assertions in the
// process-exec journey.
func jwtClaims(token string) (map[string]any, string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, "", fmt.Errorf("not a JWT")
	}
	hdrRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, "", err
	}
	var hdr struct {
		Typ string `json:"typ"`
	}
	_ = json.Unmarshal(hdrRaw, &hdr)
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, hdr.Typ, err
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, hdr.Typ, err
	}
	return claims, hdr.Typ, nil
}
