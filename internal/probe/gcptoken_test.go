package probe

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func fakeIDToken(t *testing.T, exp time.Time) string {
	t.Helper()
	return makeJWT(t, "JWT", map[string]any{"aud": "aud", "exp": exp.Unix()})
}

func TestMetadataIDToken(t *testing.T) {
	want := fakeIDToken(t, time.Now().Add(time.Hour))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			t.Errorf("missing Metadata-Flavor header")
		}
		if got := r.URL.Query().Get("audience"); got != "https://api.example" {
			t.Errorf("audience = %q", got)
		}
		if r.URL.Query().Get("format") != "full" {
			t.Errorf("format = %q", r.URL.Query().Get("format"))
		}
		w.Write([]byte(want + "\n"))
	}))
	defer ts.Close()
	old := metadataIdentityURL
	metadataIdentityURL = ts.URL
	defer func() { metadataIdentityURL = old }()

	got, err := MetadataIDToken(context.Background(), nil, "https://api.example")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("token = %q, want %q", got, want)
	}

	if _, err := MetadataIDToken(context.Background(), nil, ""); err == nil {
		t.Fatal("want error for empty audience")
	}
}

func TestMetadataIDTokenNon200(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no metadata server here", http.StatusForbidden)
	}))
	defer ts.Close()
	old := metadataIdentityURL
	metadataIdentityURL = ts.URL
	defer func() { metadataIdentityURL = old }()

	if _, err := MetadataIDToken(context.Background(), nil, "aud"); err == nil {
		t.Fatal("want error on non-200")
	}
}

func TestIDTokenSourceCaches(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// Long-lived token => second call should be served from cache.
		w.Write([]byte(fakeIDToken(t, time.Now().Add(time.Hour))))
	}))
	defer ts.Close()
	old := metadataIdentityURL
	metadataIdentityURL = ts.URL
	defer func() { metadataIdentityURL = old }()

	src := IDTokenSource(nil, "aud")
	for i := 0; i < 3; i++ {
		if _, err := src(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("metadata calls = %d, want 1 (cached)", calls.Load())
	}
}

func TestIDTokenSourceRefetchesNearExpiry(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// Expires in 2 minutes => inside the 5-minute refresh window.
		w.Write([]byte(fakeIDToken(t, time.Now().Add(2*time.Minute))))
	}))
	defer ts.Close()
	old := metadataIdentityURL
	metadataIdentityURL = ts.URL
	defer func() { metadataIdentityURL = old }()

	src := IDTokenSource(nil, "aud")
	_, _ = src(context.Background())
	_, _ = src(context.Background())
	if calls.Load() != 2 {
		t.Fatalf("metadata calls = %d, want 2 (near-expiry refetch)", calls.Load())
	}
}

func TestJWTExpiryMalformed(t *testing.T) {
	// A bogus payload segment => fallback ~10m.
	bad := base64.RawURLEncoding.EncodeToString([]byte("hdr")) + ".!!!." + "sig"
	if d := time.Until(jwtExpiry(bad)); d < 8*time.Minute || d > 12*time.Minute {
		t.Fatalf("fallback delta = %s", d)
	}
	// Valid base64 but not JSON.
	notJSON := base64.RawURLEncoding.EncodeToString([]byte("h")) + "." +
		base64.RawURLEncoding.EncodeToString([]byte("nope")) + ".s"
	if d := time.Until(jwtExpiry(notJSON)); d < 8*time.Minute || d > 12*time.Minute {
		t.Fatalf("non-JSON fallback delta = %s", d)
	}
}
