package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ignition.dev/ignition/internal/api"
	"ignition.dev/ignition/internal/auth"
	"ignition.dev/ignition/internal/config"
	"ignition.dev/ignition/internal/imagecatalog"
	"ignition.dev/ignition/internal/store"
)

type imgHarness struct {
	ts       *httptest.Server
	mem      *store.Memory
	resolver *imagecatalog.Fake
}

func newImageHarness(t *testing.T) *imgHarness {
	t.Helper()
	mem := store.NewMemory()
	mem.SeedRole("prj", "owner@corp.example", auth.RoleOwner)
	mem.SeedRole("prj", "viewer@corp.example", auth.RoleViewer)
	fake := imagecatalog.NewFake()
	fake.Images["docker.io/library/nginx:1.27"] = imagecatalog.Resolved{
		Digest:      "sha256:abc",
		RegistryRef: "docker.io/library/nginx@sha256:abc",
		Entrypoint:  []string{"/docker-entrypoint.sh"},
		Cmd:         []string{"nginx", "-g", "daemon off;"},
	}
	srv := api.NewWithResolver(config.Config{
		EnabledRegion:     "us-central1",
		GatewayURL:        "https://gateway.us-central1.ignition.dev",
		StreamTokenSecret: "test-stream-secret",
		OIDCAudience:      "https://api.ignition.dev",
	}, mem, auth.Static{Tokens: map[string]auth.Principal{
		"owner":  {Subject: "owner@corp.example", Kind: auth.KindUser},
		"viewer": {Subject: "viewer@corp.example", Kind: auth.KindUser},
	}}, fake)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &imgHarness{ts: ts, mem: mem, resolver: fake}
}

func (h *imgHarness) req(t *testing.T, method, path, token, body string) (*http.Response, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	r, err := http.NewRequest(method, h.ts.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if len(b) > 0 {
		_ = json.Unmarshal(b, &out)
	}
	return resp, out
}

func TestCreateImageResolvesAndPins(t *testing.T) {
	h := newImageHarness(t)
	resp, body := h.req(t, http.MethodPost, "/v1/projects/prj/images", "owner",
		`{"imageId":"img_nginx","sourceRef":"docker.io/library/nginx:1.27"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, body = %v", resp.StatusCode, body)
	}
	if body["digest"] != "sha256:abc" || body["registryRef"] != "docker.io/library/nginx@sha256:abc" {
		t.Fatalf("body = %v", body)
	}
	if body["state"] != "READY" {
		t.Fatalf("state = %v", body["state"])
	}
	entrypoint, _ := body["entrypoint"].([]any)
	if len(entrypoint) != 1 || entrypoint[0] != "/docker-entrypoint.sh" {
		t.Fatalf("entrypoint = %v", body["entrypoint"])
	}
	if loc := resp.Header.Get("Location"); loc != "/v1/projects/prj/images/img_nginx" {
		t.Fatalf("Location = %q", loc)
	}
}

func TestCreateImageRequiresPermission(t *testing.T) {
	h := newImageHarness(t)
	resp, _ := h.req(t, http.MethodPost, "/v1/projects/prj/images", "viewer",
		`{"imageId":"img_nginx","sourceRef":"docker.io/library/nginx:1.27"}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestCreateImageRejectsUnresolvableSourceRef(t *testing.T) {
	h := newImageHarness(t)
	resp, body := h.req(t, http.MethodPost, "/v1/projects/prj/images", "owner",
		`{"imageId":"img_bad","sourceRef":"does.not/exist:latest"}`)
	if resp.StatusCode != http.StatusBadRequest || body["code"] != "IMAGE_UNAVAILABLE" {
		t.Fatalf("status = %d, body = %v", resp.StatusCode, body)
	}
}

func TestCreateImageValidatesImageID(t *testing.T) {
	h := newImageHarness(t)
	resp, _ := h.req(t, http.MethodPost, "/v1/projects/prj/images", "owner",
		`{"imageId":"../evil","sourceRef":"docker.io/library/nginx:1.27"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestCreateImageRequiresSourceRef(t *testing.T) {
	h := newImageHarness(t)
	resp, _ := h.req(t, http.MethodPost, "/v1/projects/prj/images", "owner", `{"imageId":"img_nosrc"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestCreateImageRejectsDuplicateImageID(t *testing.T) {
	h := newImageHarness(t)
	resp, _ := h.req(t, http.MethodPost, "/v1/projects/prj/images", "owner",
		`{"imageId":"img_nginx","sourceRef":"docker.io/library/nginx:1.27"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first create: status = %d", resp.StatusCode)
	}
	resp, body := h.req(t, http.MethodPost, "/v1/projects/prj/images", "owner",
		`{"imageId":"img_nginx","sourceRef":"docker.io/library/nginx:1.27"}`)
	if resp.StatusCode != http.StatusConflict || body["code"] != "IMAGE_ALREADY_EXISTS" {
		t.Fatalf("second create: status = %d, body = %v", resp.StatusCode, body)
	}
}

func TestGetImageRoundTrip(t *testing.T) {
	h := newImageHarness(t)
	h.req(t, http.MethodPost, "/v1/projects/prj/images", "owner",
		`{"imageId":"img_nginx","sourceRef":"docker.io/library/nginx:1.27"}`)
	resp, body := h.req(t, http.MethodGet, "/v1/projects/prj/images/img_nginx", "viewer", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %v", resp.StatusCode, body)
	}
	if body["digest"] != "sha256:abc" {
		t.Fatalf("digest = %v", body["digest"])
	}
}

func TestGetImageNotFound(t *testing.T) {
	h := newImageHarness(t)
	resp, _ := h.req(t, http.MethodGet, "/v1/projects/prj/images/missing", "owner", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
