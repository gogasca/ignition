package auth

import (
	"context"
	"crypto"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const maxSkew = 60 * time.Second

// JWT validates RFC 9068-style access tokens.
// Production uses RS256 + JWKS (Key, JWKSURL, or Issuer discovery).
// HMAC is test-only and must not share the attach stream secret.
type JWT struct {
	Issuer    string
	Audience  string
	JWKSURL   string
	Key       crypto.PublicKey
	HMAC      []byte
	Algorithm string
	Now       func() time.Time
	Client    *http.Client

	mu      sync.Mutex
	jwksURL string
	keys    map[string]any
	fetched time.Time
}

func (j *JWT) Authenticate(ctx context.Context, bearer string) (Principal, error) {
	raw := strings.TrimSpace(strings.TrimPrefix(bearer, "Bearer "))
	if raw == "" {
		return Principal{}, ErrUnauthenticated
	}
	opts := []jwt.ParserOption{
		jwt.WithValidMethods([]string{j.method()}),
		jwt.WithIssuer(j.Issuer),
		jwt.WithAudience(j.Audience),
		jwt.WithLeeway(maxSkew),
		jwt.WithIssuedAt(),
		jwt.WithExpirationRequired(),
	}
	if j.Now != nil {
		opts = append(opts, jwt.WithTimeFunc(j.Now))
	}
	parser := jwt.NewParser(opts...)
	tok, err := parser.Parse(raw, func(t *jwt.Token) (any, error) {
		typ, _ := t.Header["typ"].(string)
		if !strings.EqualFold(typ, "at+jwt") {
			return nil, fmt.Errorf("%w: token type %q not at+jwt", ErrUnauthenticated, typ)
		}
		if len(j.HMAC) > 0 {
			return j.HMAC, nil
		}
		if j.Key != nil {
			return j.Key, nil
		}
		kid, _ := t.Header["kid"].(string)
		return j.keyForKID(ctx, kid)
	})
	if err != nil || tok == nil || !tok.Valid {
		return Principal{}, ErrUnauthenticated
	}
	claims, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		return Principal{}, ErrUnauthenticated
	}
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return Principal{}, ErrUnauthenticated
	}
	azp, _ := claims["azp"].(string)
	if azp == "" {
		azp, _ = claims["client_id"].(string)
	}
	return Principal{Subject: sub, Client: azp}, nil
}

func (j *JWT) method() string {
	if j.Algorithm != "" {
		return j.Algorithm
	}
	if len(j.HMAC) > 0 {
		return jwt.SigningMethodHS256.Alg()
	}
	return jwt.SigningMethodRS256.Alg()
}

func (j *JWT) keyForKID(ctx context.Context, kid string) (any, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if key, ok := j.keys[kid]; ok && time.Since(j.fetched) < 5*time.Minute {
		return key, nil
	}
	if err := j.refreshJWKS(ctx); err != nil {
		return nil, err
	}
	if kid != "" {
		if key, ok := j.keys[kid]; ok {
			return key, nil
		}
		return nil, fmt.Errorf("%w: unknown kid", ErrUnauthenticated)
	}
	if len(j.keys) == 1 {
		for _, key := range j.keys {
			return key, nil
		}
	}
	return nil, fmt.Errorf("%w: no matching JWK", ErrUnauthenticated)
}

func (j *JWT) refreshJWKS(ctx context.Context) error {
	url := j.jwksURL
	if url == "" {
		url = strings.TrimSpace(j.JWKSURL)
	}
	if url == "" {
		disc, err := j.discover(ctx)
		if err != nil {
			return err
		}
		url = disc
	}
	client := j.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	keys, err := parseJWKS(body)
	if err != nil {
		return err
	}
	j.keys = keys
	j.jwksURL = url
	j.fetched = time.Now()
	return nil
}

func (j *JWT) discover(ctx context.Context) (string, error) {
	if j.Issuer == "" {
		return "", errors.New("oidc issuer is required")
	}
	client := j.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	wellKnown := strings.TrimRight(j.Issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wellKnown, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("oidc discovery: status %d", resp.StatusCode)
	}
	var doc struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return "", err
	}
	if doc.JWKSURI == "" {
		return "", errors.New("oidc discovery missing jwks_uri")
	}
	return doc.JWKSURI, nil
}

type jwksDoc struct {
	Keys []jwk `json:"keys"`
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func parseJWKS(raw []byte) (map[string]any, error) {
	var doc jwksDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	out := map[string]any{}
	for i, k := range doc.Keys {
		if !strings.EqualFold(k.Kty, "RSA") {
			continue
		}
		if k.Use != "" && !strings.EqualFold(k.Use, "sig") {
			continue
		}
		if k.Alg != "" && !strings.EqualFold(k.Alg, jwt.SigningMethodRS256.Alg()) {
			continue
		}
		pub, err := rsaPublicFromJWK(k.N, k.E)
		if err != nil {
			return nil, err
		}
		kid := k.Kid
		if kid == "" {
			kid = fmt.Sprintf("k%d", i)
		}
		if _, exists := out[kid]; exists {
			return nil, fmt.Errorf("jwks contains duplicate kid %q", kid)
		}
		out[kid] = pub
	}
	if len(out) == 0 {
		return nil, errors.New("jwks contained no RSA signature keys")
	}
	return out, nil
}

func rsaPublicFromJWK(nB64, eB64 string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(nB64)
	if err != nil {
		return nil, err
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(eB64)
	if err != nil {
		return nil, err
	}
	if len(nBytes) == 0 || len(eBytes) == 0 {
		return nil, errors.New("invalid jwk modulus or exponent")
	}
	if new(big.Int).SetBytes(nBytes).BitLen() < 2048 {
		return nil, errors.New("jwk RSA modulus is smaller than 2048 bits")
	}
	var eInt int
	for _, b := range eBytes {
		eInt = eInt<<8 | int(b)
	}
	if eInt < 3 {
		return nil, errors.New("invalid jwk exponent")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: eInt}, nil
}
