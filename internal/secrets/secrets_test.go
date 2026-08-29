package secrets_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ignition.dev/ignition/internal/secrets"
)

func TestMapResolver(t *testing.T) {
	m := secrets.Map{"sec_a": "plain", "sec_b@1": "v1"}
	got, err := m.Resolve(context.Background(), "sec_a", "latest")
	if err != nil || got != "plain" {
		t.Fatalf("got %q err %v", got, err)
	}
	got, err = m.Resolve(context.Background(), "sec_b", "1")
	if err != nil || got != "v1" {
		t.Fatalf("got %q err %v", got, err)
	}
	if _, err := m.Resolve(context.Background(), "missing", "latest"); err == nil {
		t.Fatal("expected error")
	}
}

func TestGCPResolver(t *testing.T) {
	payload := base64.StdEncoding.EncodeToString([]byte("hello"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "service-accounts/default/token"):
			if r.Header.Get("Metadata-Flavor") != "Google" {
				http.Error(w, "flavor", 400)
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"tok"}`))
		case strings.Contains(r.URL.Path, "secrets/sec_a/versions/"):
			if r.Header.Get("Authorization") != "Bearer tok" {
				http.Error(w, "auth", 401)
				return
			}
			_, _ = w.Write([]byte(`{"payload":{"data":"` + payload + `"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	g := &secrets.GCP{
		Project:     "p",
		Client:      srv.Client(),
		MetadataURL: srv.URL + "/computeMetadata/v1/instance/service-accounts/default/token",
		APIURL:      srv.URL,
	}
	got, err := g.Resolve(context.Background(), "sec_a", "latest")
	if err != nil || got != "hello" {
		t.Fatalf("got %q err %v", got, err)
	}
}
