package auth_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"ignition.dev/ignition/internal/auth"
)

func googleClaims(now time.Time) jwt.MapClaims {
	return jwt.MapClaims{
		"iss":            "https://accounts.google.com",
		"aud":            "https://api.ignition.dev",
		"sub":            "1234567890",
		"email":          "Alice@corp.example",
		"email_verified": true,
		"hd":             "corp.example",
		"iat":            now.Unix(),
		"nbf":            now.Unix(),
		"exp":            now.Add(time.Hour).Unix(),
	}
}

func signRS256(t *testing.T, key *rsa.PrivateKey, claims jwt.MapClaims, typ string) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["typ"] = typ
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestJWTGoogleIDToken(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	newJWT := func() auth.JWT {
		return auth.JWT{
			Issuer:               "https://accounts.google.com",
			Audience:             "https://api.ignition.dev",
			Key:                  &key.PublicKey,
			Algorithm:            "RS256",
			AllowedTypes:         []string{"JWT"},
			SubjectClaim:         "email",
			RequireEmailVerified: true,
			HostedDomains:        []string{"corp.example"},
			Now:                  func() time.Time { return now },
		}
	}

	t.Run("valid user token", func(t *testing.T) {
		j := newJWT()
		p, err := j.Authenticate(context.Background(), "Bearer "+signRS256(t, key, googleClaims(now), "JWT"))
		if err != nil {
			t.Fatalf("valid: %v", err)
		}
		if p.Subject != "alice@corp.example" {
			t.Errorf("Subject = %q, want lower-cased email", p.Subject)
		}
		if p.Email != "alice@corp.example" || p.Kind != auth.KindUser || p.Domain != "corp.example" {
			t.Errorf("principal = %+v", p)
		}
	})

	t.Run("wrong typ rejected", func(t *testing.T) {
		j := newJWT()
		if _, err := j.Authenticate(context.Background(), "Bearer "+signRS256(t, key, googleClaims(now), "at+jwt")); err == nil {
			t.Fatal("typ=at+jwt accepted for Google config")
		}
	})

	t.Run("email_verified false rejected", func(t *testing.T) {
		j := newJWT()
		c := googleClaims(now)
		c["email_verified"] = false
		if _, err := j.Authenticate(context.Background(), "Bearer "+signRS256(t, key, c, "JWT")); err == nil {
			t.Fatal("unverified email accepted")
		}
	})

	t.Run("wrong hosted domain rejected", func(t *testing.T) {
		j := newJWT()
		c := googleClaims(now)
		c["hd"] = "evil.example"
		c["email"] = "mallory@evil.example"
		if _, err := j.Authenticate(context.Background(), "Bearer "+signRS256(t, key, c, "JWT")); err == nil {
			t.Fatal("foreign hosted domain accepted")
		}
	})

	t.Run("missing hosted domain rejected for user", func(t *testing.T) {
		j := newJWT()
		c := googleClaims(now)
		delete(c, "hd")
		if _, err := j.Authenticate(context.Background(), "Bearer "+signRS256(t, key, c, "JWT")); err == nil {
			t.Fatal("user token without hd accepted")
		}
	})

	t.Run("service account exempt from hosted domain", func(t *testing.T) {
		j := newJWT()
		c := googleClaims(now)
		delete(c, "hd")
		c["email"] = "prober@anyscale-demo.iam.gserviceaccount.com"
		p, err := j.Authenticate(context.Background(), "Bearer "+signRS256(t, key, c, "JWT"))
		if err != nil {
			t.Fatalf("service account token: %v", err)
		}
		if p.Kind != auth.KindServiceAccount {
			t.Errorf("Kind = %q, want service_account", p.Kind)
		}
		if p.Subject != "prober@anyscale-demo.iam.gserviceaccount.com" {
			t.Errorf("Subject = %q", p.Subject)
		}
	})

	t.Run("service account may be bound as owner", func(t *testing.T) {
		// The auth layer imposes no privilege cap on service accounts.
		if !auth.Allowed(auth.RoleOwner, auth.PermSandboxTerminate, false) {
			t.Fatal("owner role unexpectedly denied")
		}
	})
}

func TestJWTMultipleAudiences(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	j := auth.JWT{
		Issuer:       "https://accounts.google.com",
		Audiences:    []string{"https://api.staging.ignition.dev", "https://api.ignition.dev"},
		Key:          &key.PublicKey,
		Algorithm:    "RS256",
		AllowedTypes: []string{"JWT"},
		SubjectClaim: "sub",
		Now:          func() time.Time { return now },
	}

	mint := func(aud any) string {
		c := googleClaims(now)
		c["aud"] = aud
		return "Bearer " + signRS256(t, key, c, "JWT")
	}

	if _, err := j.Authenticate(context.Background(), mint("https://api.staging.ignition.dev")); err != nil {
		t.Errorf("string aud match: %v", err)
	}
	if _, err := j.Authenticate(context.Background(), mint([]string{"https://elsewhere", "https://api.ignition.dev"})); err != nil {
		t.Errorf("array aud match: %v", err)
	}
	if _, err := j.Authenticate(context.Background(), mint("https://not-allowed")); err == nil {
		t.Error("unlisted audience accepted")
	}
}

func TestJWTIAPAssertionES256(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	x := base64.RawURLEncoding.EncodeToString(key.X.Bytes())
	y := base64.RawURLEncoding.EncodeToString(key.Y.Bytes())
	mux := http.NewServeMux()
	mux.HandleFunc("/iap-keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{{
				"kty": "EC", "crv": "P-256", "kid": "iap1", "use": "sig", "alg": "ES256", "x": x, "y": y,
			}},
		})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	now := time.Now()
	j := auth.JWT{
		Issuer:       "https://cloud.google.com/iap",
		Audience:     "/projects/123/global/backendServices/456",
		JWKSURL:      ts.URL + "/iap-keys",
		Algorithm:    "ES256",
		AllowedTypes: []string{"JWT", ""},
		SubjectClaim: "email",
		Client:       ts.Client(),
	}

	tok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"iss":   "https://cloud.google.com/iap",
		"aud":   "/projects/123/global/backendServices/456",
		"sub":   "accounts.google.com:1122334455",
		"email": "bob@corp.example",
		"hd":    "corp.example",
		"iat":   now.Unix(),
		"nbf":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	})
	tok.Header["kid"] = "iap1"
	raw, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}

	p, err := j.Authenticate(context.Background(), raw)
	if err != nil {
		t.Fatalf("IAP assertion: %v", err)
	}
	if p.Subject != "bob@corp.example" || p.Domain != "corp.example" || p.Kind != auth.KindUser {
		t.Fatalf("principal = %+v", p)
	}
}

func TestChainFallback(t *testing.T) {
	ok := stubAuth{principal: auth.Principal{Subject: "svc@x.iam.gserviceaccount.com"}}
	no := stubAuth{err: auth.ErrUnauthenticated}
	boom := stubAuth{err: errBoom}

	p, err := (auth.Chain{no, ok}).Authenticate(context.Background(), "tok")
	if err != nil || p.Subject != "svc@x.iam.gserviceaccount.com" {
		t.Fatalf("fallback to second: p=%+v err=%v", p, err)
	}
	if _, err := (auth.Chain{no, no}).Authenticate(context.Background(), "tok"); err == nil {
		t.Fatal("empty chain result should be unauthenticated")
	}
	if _, err := (auth.Chain{boom, ok}).Authenticate(context.Background(), "tok"); err != errBoom {
		t.Fatalf("non-unauthenticated error should short-circuit, got %v", err)
	}
}

func TestClassifySubject(t *testing.T) {
	cases := map[string]auth.SubjectKind{
		"":                                  "",
		"alice@corp.example":                auth.KindUser,
		"svc@proj.iam.gserviceaccount.com":  auth.KindServiceAccount,
		"SVC@PROJ.IAM.GSERVICEACCOUNT.COM":  auth.KindServiceAccount,
		"bot@developer.gserviceaccount.com": auth.KindServiceAccount,
	}
	for in, want := range cases {
		if got := auth.ClassifySubject(in); got != want {
			t.Errorf("ClassifySubject(%q) = %q, want %q", in, got, want)
		}
	}
}

var errBoom = errBoomType{}

type errBoomType struct{}

func (errBoomType) Error() string { return "boom" }

type stubAuth struct {
	principal auth.Principal
	err       error
}

func (s stubAuth) Authenticate(context.Context, string) (auth.Principal, error) {
	return s.principal, s.err
}
