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
	"ignition.dev/ignition/internal/store"
)

type rbHarness struct {
	ts  *httptest.Server
	mem *store.Memory
}

func newRBHarness(t *testing.T) *rbHarness {
	t.Helper()
	mem := store.NewMemory()
	mem.SeedRole("prj", "owner@corp.example", auth.RoleOwner)
	mem.SeedRole("prj", "admin@corp.example", auth.RoleAdmin)
	mem.SeedRole("prj", "dev@corp.example", auth.RoleDeveloper)
	srv := api.New(config.Config{
		EnabledRegion:     "us-central1",
		GatewayURL:        "https://gateway.us-central1.ignition.dev",
		StreamTokenSecret: "test-stream-secret",
		OIDCAudience:      "https://api.ignition.dev",
	}, mem, auth.Static{Tokens: map[string]auth.Principal{
		"owner": {Subject: "owner@corp.example", Kind: auth.KindUser},
		"admin": {Subject: "admin@corp.example", Kind: auth.KindUser},
		"dev":   {Subject: "dev@corp.example", Kind: auth.KindUser},
	}})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &rbHarness{ts: ts, mem: mem}
}

func (h *rbHarness) req(t *testing.T, method, path, token, body string) (*http.Response, map[string]any) {
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

func TestRoleBindingCRUD(t *testing.T) {
	h := newRBHarness(t)

	// Create.
	resp, body := h.req(t, http.MethodPut, "/v1/projects/prj/roleBindings/carol@corp.example", "owner", `{"role":"developer"}`)
	if resp.StatusCode != http.StatusOK || body["subject"] != "carol@corp.example" || body["role"] != "developer" {
		t.Fatalf("put: %d %v", resp.StatusCode, body)
	}

	// Read back via list.
	resp, body = h.req(t, http.MethodGet, "/v1/projects/prj/roleBindings", "admin", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: %d", resp.StatusCode)
	}
	list, _ := body["roleBindings"].([]any)
	if len(list) != 4 {
		t.Fatalf("list has %d bindings, want 4: %v", len(list), list)
	}

	// Read back single.
	resp, body = h.req(t, http.MethodGet, "/v1/projects/prj/roleBindings/carol@corp.example", "owner", "")
	if resp.StatusCode != http.StatusOK || body["role"] != "developer" {
		t.Fatalf("get: %d %v", resp.StatusCode, body)
	}

	// Update in place.
	resp, body = h.req(t, http.MethodPut, "/v1/projects/prj/roleBindings/carol@corp.example", "owner", `{"role":"operator"}`)
	if resp.StatusCode != http.StatusOK || body["role"] != "operator" {
		t.Fatalf("update: %d %v", resp.StatusCode, body)
	}

	// Delete.
	resp, _ = h.req(t, http.MethodDelete, "/v1/projects/prj/roleBindings/carol@corp.example", "owner", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	resp, _ = h.req(t, http.MethodDelete, "/v1/projects/prj/roleBindings/carol@corp.example", "owner", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete missing: %d", resp.StatusCode)
	}
}

func TestRoleBindingAuthz(t *testing.T) {
	h := newRBHarness(t)

	// Developer cannot manage bindings.
	if resp, _ := h.req(t, http.MethodPut, "/v1/projects/prj/roleBindings/x@corp.example", "dev", `{"role":"viewer"}`); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("dev put: %d, want 403", resp.StatusCode)
	}
	if resp, _ := h.req(t, http.MethodGet, "/v1/projects/prj/roleBindings", "dev", ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("dev list: %d, want 403", resp.StatusCode)
	}
	// Unknown project (caller has no binding) -> 404.
	if resp, _ := h.req(t, http.MethodGet, "/v1/projects/other/roleBindings", "owner", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown project: %d, want 404", resp.StatusCode)
	}
}

func TestRoleBindingValidation(t *testing.T) {
	h := newRBHarness(t)

	cases := []struct {
		subject, body string
		want          int
	}{
		{"carol@corp.example", `{"role":"superuser"}`, http.StatusBadRequest},
		{"carol@corp.example", `not json`, http.StatusBadRequest},
		{"group:eng@corp.example", `{"role":"viewer"}`, http.StatusBadRequest},
		{"notanemail", `{"role":"viewer"}`, http.StatusBadRequest},
		{"domain:corp", `{"role":"viewer"}`, http.StatusBadRequest},
		{"domain:corp.example", `{"role":"viewer"}`, http.StatusOK},
	}
	for _, c := range cases {
		resp, _ := h.req(t, http.MethodPut, "/v1/projects/prj/roleBindings/"+c.subject, "owner", c.body)
		if resp.StatusCode != c.want {
			t.Errorf("PUT %s %s -> %d, want %d", c.subject, c.body, resp.StatusCode, c.want)
		}
	}
}

func TestRoleBindingLastOwnerGuard(t *testing.T) {
	h := newRBHarness(t)

	// owner@corp.example is the only owner: downgrade is refused.
	if resp, _ := h.req(t, http.MethodPut, "/v1/projects/prj/roleBindings/owner@corp.example", "owner", `{"role":"admin"}`); resp.StatusCode != http.StatusConflict {
		t.Fatalf("downgrade last owner: %d, want 409", resp.StatusCode)
	}
	if resp, _ := h.req(t, http.MethodDelete, "/v1/projects/prj/roleBindings/owner@corp.example", "owner", ""); resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete last owner: %d, want 409", resp.StatusCode)
	}

	// Add a second owner, then the first can be removed.
	if resp, _ := h.req(t, http.MethodPut, "/v1/projects/prj/roleBindings/owner2@corp.example", "owner", `{"role":"owner"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("add second owner: %d", resp.StatusCode)
	}
	if resp, _ := h.req(t, http.MethodDelete, "/v1/projects/prj/roleBindings/owner@corp.example", "admin", ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("remove first owner: %d, want 204", resp.StatusCode)
	}
}
