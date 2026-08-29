package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"ignition.dev/ignition/internal/auth"
)

func TestStaticAuthenticate(t *testing.T) {
	a := auth.Static{Tokens: map[string]auth.Principal{"tok": {Subject: "alice"}}}
	p, err := a.Authenticate(context.Background(), "Bearer tok")
	if err != nil || p.Subject != "alice" {
		t.Fatalf("got %+v err=%v", p, err)
	}
	if _, err := a.Authenticate(context.Background(), "Bearer nope"); err == nil {
		t.Fatal("expected unauthenticated")
	}
}

func TestAllowedRBAC(t *testing.T) {
	tests := []struct {
		role string
		perm auth.Permission
		own  bool
		want bool
	}{
		{auth.RoleViewer, auth.PermSandboxGet, false, true},
		{auth.RoleViewer, auth.PermSandboxCreate, false, false},
		{auth.RoleDeveloper, auth.PermSandboxCreate, false, true},
		{auth.RoleDeveloper, auth.PermSandboxTerminate, false, false},
		{auth.RoleDeveloper, auth.PermSandboxTerminate, true, true},
		{auth.RoleDeveloper, auth.PermOperationCancel, true, true},
		{auth.RoleOwner, auth.PermSandboxTerminate, false, true},
		{auth.RoleOperator, auth.PermSandboxExec, false, true},
	}
	for _, tc := range tests {
		if got := auth.Allowed(tc.role, tc.perm, tc.own); got != tc.want {
			t.Fatalf("Allowed(%s,%s,own=%v)=%v want %v", tc.role, tc.perm, tc.own, got, tc.want)
		}
	}
}

func mintAccess(t *testing.T, key *rsa.PrivateKey, claims jwt.MapClaims, typ string) string {
	t.Helper()
	if _, ok := claims["nbf"]; !ok {
		if iat, ok := claims["iat"].(int64); ok {
			claims["nbf"] = iat
		}
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	if typ != "" {
		tok.Header["typ"] = typ
	}
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestJWTAuthenticate(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 27, 17, 0, 0, 0, time.UTC)
	j := auth.JWT{
		Issuer:   "https://issuer.example",
		Audience: "https://api.ignition.dev",
		Key:      &key.PublicKey,
		Now:      func() time.Time { return now },
	}
	valid := jwt.MapClaims{
		"iss": "https://issuer.example",
		"aud": "https://api.ignition.dev",
		"sub": "user-1",
		"azp": "cli",
		"iat": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	}
	p, err := j.Authenticate(context.Background(), "Bearer "+mintAccess(t, key, valid, "at+jwt"))
	if err != nil {
		t.Fatalf("valid token: %v", err)
	}
	if p.Subject != "user-1" || p.Client != "cli" {
		t.Fatalf("principal = %+v", p)
	}

	wrongAud := jwt.MapClaims{
		"iss": "https://issuer.example",
		"aud": "https://other",
		"sub": "user-1",
		"iat": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	}
	if _, err := j.Authenticate(context.Background(), "Bearer "+mintAccess(t, key, wrongAud, "at+jwt")); err == nil {
		t.Fatal("wrong audience accepted")
	}

	expired := jwt.MapClaims{
		"iss": "https://issuer.example",
		"aud": "https://api.ignition.dev",
		"sub": "user-1",
		"iat": now.Add(-2 * time.Hour).Unix(),
		"exp": now.Add(-time.Hour).Unix(),
	}
	if _, err := j.Authenticate(context.Background(), "Bearer "+mintAccess(t, key, expired, "at+jwt")); err == nil {
		t.Fatal("expired token accepted")
	}

	if _, err := j.Authenticate(context.Background(), "Bearer "+mintAccess(t, key, valid, "JWT")); err == nil {
		t.Fatal("typ=JWT accepted")
	}

	stream := jwt.MapClaims{
		"iss": "ignition-api",
		"aud": "https://gateway.us-central1.ignition.dev",
		"sub": "user-1",
		"iat": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	}
	if _, err := j.Authenticate(context.Background(), "Bearer "+mintAccess(t, key, stream, "stream+jwt")); err == nil {
		t.Fatal("stream token accepted as API access token")
	}
}

func TestJWTHMAC(t *testing.T) {
	now := time.Now()
	j := auth.JWT{
		Issuer:    "https://issuer.example",
		Audience:  "https://api.ignition.dev",
		HMAC:      []byte("hmac-secret"),
		Algorithm: "HS256",
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss": "https://issuer.example",
		"aud": "https://api.ignition.dev",
		"sub": "hmac-user",
		"iat": now.Unix(),
		"nbf": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	})
	tok.Header["typ"] = "at+jwt"
	raw, err := tok.SignedString([]byte("hmac-secret"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := j.Authenticate(context.Background(), "Bearer "+raw)
	if err != nil || p.Subject != "hmac-user" {
		t.Fatalf("got %+v err=%v", p, err)
	}
}

func TestJWTJWKS(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	n := base64.RawURLEncoding.EncodeToString(key.N.Bytes())
	eBytes := big.NewInt(int64(key.E)).Bytes()
	e := base64.RawURLEncoding.EncodeToString(eBytes)
	mux := http.NewServeMux()
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{{
				"kty": "RSA", "kid": "k1", "use": "sig", "alg": "RS256", "n": n, "e": e,
			}},
		})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	now := time.Now()
	j := &auth.JWT{
		Issuer:   "https://issuer.example",
		Audience: "https://api.ignition.dev",
		JWKSURL:  ts.URL + "/keys",
		Client:   ts.Client(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": "https://issuer.example",
		"aud": "https://api.ignition.dev",
		"sub": "jwks-user",
		"iat": now.Unix(),
		"nbf": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	})
	tok.Header["typ"] = "at+jwt"
	tok.Header["kid"] = "k1"
	raw, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	p, err := j.Authenticate(context.Background(), "Bearer "+raw)
	if err != nil || p.Subject != "jwks-user" {
		t.Fatalf("got %+v err=%v", p, err)
	}
}
