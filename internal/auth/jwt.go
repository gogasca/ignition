package auth

import (
	"context"
	"crypto"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const maxSkew = 60 * time.Second

// JWT validates signed access / identity tokens.
//
//   - First-party RFC 9068 access tokens: RS256 + JWKS, typ=at+jwt (the default).
//   - Google ID tokens (service accounts and impersonated users): RS256,
//     typ=JWT, issuer https://accounts.google.com; set SubjectClaim="email"
//     and RequireEmailVerified.
//   - Cloud IAP assertions: ES256, issuer https://cloud.google.com/iap; set
//     Algorithm="ES256" and AllowedTypes to accept the IAP header type.
//
// HMAC is test-only and must not share the attach stream secret.
type JWT struct {
	Issuer    string
	Audience  string   // single expected audience (back-compat)
	Audiences []string // additional accepted audiences; any match passes
	JWKSURL   string
	Key       crypto.PublicKey
	HMAC      []byte
	Algorithm string
	Now       func() time.Time
	Client    *http.Client

	// AllowedTypes restricts the JWT `typ` header (case-insensitive). Empty
	// defaults to {"at+jwt"}. Include "" to accept a token with no typ header.
	AllowedTypes []string
	// SubjectClaim selects the claim that becomes Principal.Subject:
	// "sub" (default) or "email".
	SubjectClaim string
	// RequireEmailVerified rejects a token whose `email_verified` claim is
	// not true. Use for Google ID tokens; leave false for IAP assertions,
	// which do not carry the claim.
	RequireEmailVerified bool
	// HostedDomains, when non-empty, requires a user token's `hd` claim to
	// be one of these Workspace domains. Service-account tokens (no `hd`)
	// are exempt.
	HostedDomains []string

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
		jwt.WithLeeway(maxSkew),
		jwt.WithIssuedAt(),
		jwt.WithExpirationRequired(),
	}
	if j.Now != nil {
		opts = append(opts, jwt.WithTimeFunc(j.Now))
	}
	parser := jwt.NewParser(opts...)
	tok, err := parser.Parse(raw, func(t *jwt.Token) (any, error) {
		if !j.typeAllowed(t) {
			typ, _ := t.Header["typ"].(string)
			return nil, fmt.Errorf("%w: token type %q not accepted", ErrUnauthenticated, typ)
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
	if !j.audienceOK(claims) {
		return Principal{}, ErrUnauthenticated
	}
	return j.principal(claims)
}

// principal derives a Principal from validated claims, enforcing the
// email/hosted-domain policy.
func (j *JWT) principal(claims jwt.MapClaims) (Principal, error) {
	sub, _ := claims["sub"].(string)
	email := strings.ToLower(strings.TrimSpace(stringClaim(claims["email"])))
	hd := strings.TrimSpace(stringClaim(claims["hd"]))
	kind := ClassifySubject(email)

	if j.RequireEmailVerified && !boolClaim(claims["email_verified"]) {
		return Principal{}, ErrUnauthenticated
	}
	if len(j.HostedDomains) > 0 && kind == KindUser {
		if hd == "" || !slices.Contains(j.HostedDomains, hd) {
			return Principal{}, ErrUnauthenticated
		}
	}

	var subject string
	switch j.SubjectClaim {
	case "email":
		subject = email
	default:
		subject = sub
	}
	if subject == "" {
		return Principal{}, ErrUnauthenticated
	}

	azp, _ := claims["azp"].(string)
	if azp == "" {
		azp, _ = claims["client_id"].(string)
	}
	return Principal{
		Subject: subject,
		Email:   email,
		Kind:    kind,
		Domain:  hd,
		Client:  azp,
	}, nil
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

func (j *JWT) allowedTypes() []string {
	if len(j.AllowedTypes) == 0 {
		return []string{"at+jwt"}
	}
	return j.AllowedTypes
}

func (j *JWT) typeAllowed(t *jwt.Token) bool {
	typ, _ := t.Header["typ"].(string)
	for _, want := range j.allowedTypes() {
		if strings.EqualFold(typ, want) {
			return true
		}
	}
	return false
}

func (j *JWT) wantAudiences() []string {
	out := append([]string{}, j.Audiences...)
	if j.Audience != "" {
		out = append(out, j.Audience)
	}
	return out
}

// audienceOK requires the token's `aud` to intersect the configured set. An
// empty configured set accepts any audience (config is expected to set one).
func (j *JWT) audienceOK(claims jwt.MapClaims) bool {
	want := j.wantAudiences()
	if len(want) == 0 {
		return true
	}
	for _, have := range claimAudiences(claims) {
		if slices.Contains(want, have) {
			return true
		}
	}
	return false
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
	keys, err := parseJWKS(body, j.method())
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
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// parseJWKS builds a kid -> public key map, keeping only signature keys whose
// algorithm matches wantAlg (RSA for RS*, EC for ES*).
func parseJWKS(raw []byte, wantAlg string) (map[string]any, error) {
	var doc jwksDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	out := map[string]any{}
	for i, k := range doc.Keys {
		if k.Use != "" && !strings.EqualFold(k.Use, "sig") {
			continue
		}
		if k.Alg != "" && !strings.EqualFold(k.Alg, wantAlg) {
			continue
		}
		var (
			pub any
			err error
		)
		switch {
		case strings.EqualFold(k.Kty, "RSA"):
			pub, err = rsaPublicFromJWK(k.N, k.E)
		case strings.EqualFold(k.Kty, "EC"):
			pub, err = ecPublicFromJWK(k.Crv, k.X, k.Y)
		default:
			continue
		}
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
		return nil, errors.New("jwks contained no usable signature keys")
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

// ecPublicFromJWK decodes an EC signature key, validating that the point is on
// the named curve via crypto/ecdh (which rejects off-curve and identity points).
func ecPublicFromJWK(crv, xB64, yB64 string) (*ecdsa.PublicKey, error) {
	var (
		curve elliptic.Curve
		ecdhC ecdh.Curve
		size  int
	)
	switch crv {
	case "P-256":
		curve, ecdhC, size = elliptic.P256(), ecdh.P256(), 32
	case "P-384":
		curve, ecdhC, size = elliptic.P384(), ecdh.P384(), 48
	case "P-521":
		curve, ecdhC, size = elliptic.P521(), ecdh.P521(), 66
	default:
		return nil, fmt.Errorf("unsupported EC curve %q", crv)
	}
	x, err := base64.RawURLEncoding.DecodeString(xB64)
	if err != nil {
		return nil, err
	}
	y, err := base64.RawURLEncoding.DecodeString(yB64)
	if err != nil {
		return nil, err
	}
	if len(x) != size || len(y) != size {
		return nil, errors.New("invalid EC coordinate length")
	}
	sec1 := make([]byte, 0, 1+2*size)
	sec1 = append(sec1, 0x04)
	sec1 = append(sec1, x...)
	sec1 = append(sec1, y...)
	if _, err := ecdhC.NewPublicKey(sec1); err != nil {
		return nil, fmt.Errorf("invalid EC public key: %w", err)
	}
	return &ecdsa.PublicKey{
		Curve: curve,
		X:     new(big.Int).SetBytes(x),
		Y:     new(big.Int).SetBytes(y),
	}, nil
}

func claimAudiences(claims jwt.MapClaims) []string {
	switch v := claims["aud"].(type) {
	case string:
		return []string{v}
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func stringClaim(v any) string {
	s, _ := v.(string)
	return s
}

func boolClaim(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return strings.EqualFold(b, "true")
	default:
		return false
	}
}
