package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"ignition.dev/ignition/internal/api"
	"ignition.dev/ignition/internal/auth"
	"ignition.dev/ignition/internal/config"
	"ignition.dev/ignition/internal/store"
)

const (
	createBody = `{
		"imageId": "img_seed",
		"resources": {
			"cpuMilli": 1000,
			"memoryMiB": 2048,
			"gpu": {"count": 1, "type": "NVIDIA_L4"}
		}
	}`
)

type harness struct {
	ts  *httptest.Server
	mem *store.Memory
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	mem := store.NewMemory()
	mem.SeedRole("prj_dev", "alice", auth.RoleOwner)
	mem.SeedRole("prj_dev", "bob", auth.RoleDeveloper)
	mem.SeedRole("prj_dev", "viewer", auth.RoleViewer)
	mem.SeedImage("prj_dev", "img_seed")
	srv := api.New(config.Config{
		EnabledRegion:      "us-central1",
		GatewayURL:         "https://gateway.us-central1.ignition.dev",
		StreamTokenSecret:  "test-stream-secret",
		MaxActiveSandboxes: 2,
		OIDCAudience:       "https://api.ignition.dev",
	}, mem, auth.Static{Tokens: map[string]auth.Principal{
		"alice":  {Subject: "alice"},
		"bob":    {Subject: "bob"},
		"viewer": {Subject: "viewer"},
	}})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &harness{ts: ts, mem: mem}
}

func (h *harness) do(t *testing.T, method, path, token, idemKey, body string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, h.ts.URL+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func decode(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode %d: %v", resp.StatusCode, err)
	}
	return out
}

func TestHealthzUnauthenticated(t *testing.T) {
	h := newHarness(t)
	resp := h.do(t, http.MethodGet, "/healthz", "", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestMissingBearerIs401(t *testing.T) {
	h := newHarness(t)
	resp := h.do(t, http.MethodGet, "/v1/projects/prj_dev/sandboxes", "", "", "")
	body := decode(t, resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if body["code"] != "UNAUTHENTICATED" {
		t.Fatalf("code = %v", body["code"])
	}
}

func TestCreateSandboxAccepted(t *testing.T) {
	h := newHarness(t)
	resp := h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "idem-1", createBody)
	body := decode(t, resp)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d body=%v", resp.StatusCode, body)
	}
	sb, _ := body["sandbox"].(map[string]any)
	op, _ := body["operation"].(map[string]any)
	if sb["state"] != "CREATING" {
		t.Fatalf("sandbox.state = %v", sb["state"])
	}
	if op["kind"] != "CREATE_SANDBOX" {
		t.Fatalf("operation.kind = %v", op["kind"])
	}
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "/sandboxes/"+sb["id"].(string)) {
		t.Fatalf("Location = %q", loc)
	}
}

func TestCreateSandboxIdempotentReplay(t *testing.T) {
	h := newHarness(t)
	a := decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "same-key", createBody))
	b := decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "same-key", createBody))
	idA := a["sandbox"].(map[string]any)["id"]
	idB := b["sandbox"].(map[string]any)["id"]
	if idA != idB {
		t.Fatalf("replay produced %v, want %v", idB, idA)
	}
}

func TestCreateSandboxIdempotencyKeyReused(t *testing.T) {
	h := newHarness(t)
	_ = decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "reuse", createBody))
	other := strings.Replace(createBody, "1000", "2000", 1)
	resp := h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "reuse", other)
	body := decode(t, resp)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if body["code"] != "IDEMPOTENCY_KEY_REUSED" {
		t.Fatalf("code = %v", body["code"])
	}
}

func TestCreateSandboxRejectsGPUCount(t *testing.T) {
	h := newHarness(t)
	body := `{
		"imageId": "img_seed",
		"resources": {"cpuMilli": 1, "memoryMiB": 1, "gpu": {"count": 2, "type": "NVIDIA_L4"}}
	}`
	resp := h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "gpu", body)
	out := decode(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if out["code"] != "INVALID_ARGUMENT" {
		t.Fatalf("code = %v", out["code"])
	}
}

func TestCreateSandboxRejectsUnknownGPUType(t *testing.T) {
	h := newHarness(t)
	body := `{
		"imageId": "img_seed",
		"resources": {"cpuMilli": 1, "memoryMiB": 1, "gpu": {"count": 1, "type": "nvidia-l4"}}
	}`
	resp := h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "gputype", body)
	out := decode(t, resp)
	if resp.StatusCode != http.StatusBadRequest || out["code"] != "INVALID_ARGUMENT" {
		t.Fatalf("status=%d code=%v", resp.StatusCode, out["code"])
	}
}

func TestCreateSandboxUnknownRegion(t *testing.T) {
	h := newHarness(t)
	body := `{
		"imageId": "img_seed",
		"resources": {"cpuMilli": 1, "memoryMiB": 1, "gpu": {"count": 1, "type": "NVIDIA_L4"}},
		"placement": {"region": "europe-west1"}
	}`
	resp := h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "region", body)
	out := decode(t, resp)
	if resp.StatusCode != http.StatusBadRequest || out["code"] != "INVALID_ARGUMENT" {
		t.Fatalf("status=%d code=%v", resp.StatusCode, out["code"])
	}
}

func TestCreateSandboxReservedLabel(t *testing.T) {
	h := newHarness(t)
	body := `{
		"imageId": "img_seed",
		"resources": {"cpuMilli": 1, "memoryMiB": 1, "gpu": {"count": 1, "type": "NVIDIA_L4"}},
		"labels": {"ignition.internal": "x"}
	}`
	resp := h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "label", body)
	out := decode(t, resp)
	if resp.StatusCode != http.StatusBadRequest || out["code"] != "INVALID_ARGUMENT" {
		t.Fatalf("status=%d code=%v", resp.StatusCode, out["code"])
	}
}

func TestCreateSandboxMissingIdempotencyKey(t *testing.T) {
	h := newHarness(t)
	resp := h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "", createBody)
	out := decode(t, resp)
	if resp.StatusCode != http.StatusBadRequest || out["code"] != "INVALID_ARGUMENT" {
		t.Fatalf("status=%d code=%v", resp.StatusCode, out["code"])
	}
}

func TestCreateSandboxQuotaExceeded(t *testing.T) {
	mem := store.NewMemory()
	mem.SeedRole("prj_dev", "alice", auth.RoleOwner)
	mem.SeedImage("prj_dev", "img_seed")
	srv := api.New(config.Config{
		EnabledRegion:      "us-central1",
		StreamTokenSecret:  "s",
		MaxActiveSandboxes: 1,
	}, mem, auth.Static{Tokens: map[string]auth.Principal{"alice": {Subject: "alice"}}})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	h := &harness{ts: ts, mem: mem}
	if st := h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "q1", createBody); st.StatusCode != http.StatusAccepted {
		t.Fatalf("first create: %d", st.StatusCode)
	} else {
		st.Body.Close()
	}
	resp := h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "q2", createBody)
	out := decode(t, resp)
	if resp.StatusCode != http.StatusTooManyRequests || out["code"] != "QUOTA_EXCEEDED" {
		t.Fatalf("status=%d code=%v", resp.StatusCode, out["code"])
	}
}

func TestViewerCannotCreate(t *testing.T) {
	h := newHarness(t)
	resp := h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "viewer", "v1", createBody)
	out := decode(t, resp)
	if resp.StatusCode != http.StatusForbidden || out["code"] != "PERMISSION_DENIED" {
		t.Fatalf("status=%d code=%v", resp.StatusCode, out["code"])
	}
}

func TestCrossProjectIs404(t *testing.T) {
	h := newHarness(t)
	resp := h.do(t, http.MethodGet, "/v1/projects/prj_other/sandboxes/sbx_nope", "alice", "", "")
	out := decode(t, resp)
	if resp.StatusCode != http.StatusNotFound || out["code"] != "NOT_FOUND" {
		t.Fatalf("status=%d code=%v", resp.StatusCode, out["code"])
	}
}

func TestGetListTerminateAndWatchSandbox(t *testing.T) {
	h := newHarness(t)
	created := decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "life", createBody))
	id := created["sandbox"].(map[string]any)["id"].(string)

	got := decode(t, h.do(t, http.MethodGet, "/v1/projects/prj_dev/sandboxes/"+id, "alice", "", ""))
	if got["id"] != id {
		t.Fatalf("get id = %v", got["id"])
	}

	listed := decode(t, h.do(t, http.MethodGet, "/v1/projects/prj_dev/sandboxes", "alice", "", ""))
	items, _ := listed["sandboxes"].([]any)
	if len(items) != 1 {
		t.Fatalf("list len = %d", len(items))
	}

	term := h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes/"+id+":terminate", "alice", "term-1", "{}")
	termBody := decode(t, term)
	if term.StatusCode != http.StatusAccepted {
		t.Fatalf("terminate status = %d body=%v", term.StatusCode, termBody)
	}
	if termBody["sandbox"].(map[string]any)["state"] != "TERMINATING" {
		t.Fatalf("state after terminate = %v", termBody["sandbox"])
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.ts.URL+"/v1/projects/prj_dev/sandboxes/"+id+":watch", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer alice")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("watch status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(raw, []byte("event: snapshot")) {
		t.Fatalf("watch body = %s", raw)
	}
}

func TestDeveloperCannotTerminateOthers(t *testing.T) {
	h := newHarness(t)
	created := decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "own", createBody))
	id := created["sandbox"].(map[string]any)["id"].(string)
	resp := h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes/"+id+":terminate", "bob", "bob-term", "{}")
	out := decode(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d code=%v", resp.StatusCode, out["code"])
	}
}

func TestDeveloperCanTerminateOwn(t *testing.T) {
	h := newHarness(t)
	created := decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "bob", "bob-own", createBody))
	id := created["sandbox"].(map[string]any)["id"].(string)
	resp := h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes/"+id+":terminate", "bob", "bob-term", "{}")
	out := decode(t, resp)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d body=%v", resp.StatusCode, out)
	}
}

func TestProcessRequiresReadySandbox(t *testing.T) {
	h := newHarness(t)
	created := decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "prc-early", createBody))
	id := created["sandbox"].(map[string]any)["id"].(string)
	resp := h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes/"+id+"/processes", "alice", "p1", `{"command":["sleep","1"]}`)
	out := decode(t, resp)
	if resp.StatusCode != http.StatusBadRequest || out["code"] != "FAILED_PRECONDITION" {
		t.Fatalf("status=%d code=%v", resp.StatusCode, out["code"])
	}
}

func TestProcessLifecycleAndAttachToken(t *testing.T) {
	h := newHarness(t)
	created := decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "prc-ok", createBody))
	sbx := created["sandbox"].(map[string]any)["id"].(string)
	h.mem.SetSandboxState("prj_dev", sbx, "READY")

	proc := decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes/"+sbx+"/processes", "alice", "p-create", `{"command":["bash","-lc","true"]}`))
	if proc["state"] != "CREATING" {
		t.Fatalf("process state = %v", proc["state"])
	}
	prc := proc["id"].(string)

	got := decode(t, h.do(t, http.MethodGet, "/v1/projects/prj_dev/sandboxes/"+sbx+"/processes/"+prc, "alice", "", ""))
	if got["id"] != prc {
		t.Fatalf("get process = %v", got["id"])
	}

	listed := decode(t, h.do(t, http.MethodGet, "/v1/projects/prj_dev/sandboxes/"+sbx+"/processes", "alice", "", ""))
	if len(listed["processes"].([]any)) != 1 {
		t.Fatalf("list processes = %v", listed)
	}

	att := decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes/"+sbx+"/processes/"+prc+":attach", "alice", "att-1", "{}"))
	if att["gatewayUrl"] != "https://gateway.us-central1.ignition.dev" {
		t.Fatalf("gatewayUrl = %v", att["gatewayUrl"])
	}
	token, _ := att["streamToken"].(string)
	if token == "" {
		t.Fatal("missing streamToken")
	}
	parsed, _, err := jwt.NewParser().ParseUnverified(token, jwt.MapClaims{})
	if err != nil {
		t.Fatalf("parse token: %v", err)
	}
	if typ, _ := parsed.Header["typ"].(string); typ != "stream+jwt" {
		t.Fatalf("typ = %q", typ)
	}
	claims := parsed.Claims.(jwt.MapClaims)
	aud, _ := claims.GetAudience()
	for _, a := range aud {
		if a == "https://api.ignition.dev" {
			t.Fatal("stream token audience must not be the API audience")
		}
	}
	if claims["action"] != "attach" || claims["process_id"] != prc {
		t.Fatalf("claims = %v", claims)
	}

	sig := decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes/"+sbx+"/processes/"+prc+":signal", "alice", "sig-1", `{"signal":"SIGTERM"}`))
	if sig["terminatingSignal"] != "SIGTERM" {
		t.Fatalf("signal = %v", sig)
	}
	can := decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes/"+sbx+"/processes/"+prc+":cancel", "alice", "can-1", "{}"))
	if can["state"] != "CANCELLING" {
		t.Fatalf("cancel state = %v", can["state"])
	}
}

func TestAttachRejectedWhenSandboxNotReady(t *testing.T) {
	h := newHarness(t)
	created := decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "att-early", createBody))
	sbx := created["sandbox"].(map[string]any)["id"].(string)
	h.mem.SetSandboxState("prj_dev", sbx, "READY")
	proc := decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes/"+sbx+"/processes", "alice", "p-att", `{"command":["true"]}`))
	prc := proc["id"].(string)
	h.mem.SetSandboxState("prj_dev", sbx, "STARTED")
	resp := h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes/"+sbx+"/processes/"+prc+":attach", "alice", "att-2", "{}")
	out := decode(t, resp)
	if resp.StatusCode != http.StatusBadRequest || out["code"] != "FAILED_PRECONDITION" {
		t.Fatalf("status=%d code=%v", resp.StatusCode, out["code"])
	}
}

func TestOperationsGetListCancel(t *testing.T) {
	h := newHarness(t)
	created := decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "ops", createBody))
	opID := created["operation"].(map[string]any)["id"].(string)
	sbx := created["sandbox"].(map[string]any)["id"].(string)

	got := decode(t, h.do(t, http.MethodGet, "/v1/projects/prj_dev/operations/"+opID, "alice", "", ""))
	if got["id"] != opID {
		t.Fatalf("get operation = %v", got["id"])
	}
	listed := decode(t, h.do(t, http.MethodGet, "/v1/projects/prj_dev/operations?resourceId="+sbx, "alice", "", ""))
	if len(listed["operations"].([]any)) != 1 {
		t.Fatalf("list = %v", listed)
	}
	cancelled := decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/operations/"+opID+":cancel", "alice", "op-can", "{}"))
	if cancelled["state"] != "CANCELLED" {
		t.Fatalf("cancel state = %v", cancelled["state"])
	}
}

func TestViewerCannotCancelOperation(t *testing.T) {
	h := newHarness(t)
	created := decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "ops-v", createBody))
	opID := created["operation"].(map[string]any)["id"].(string)
	resp := h.do(t, http.MethodPost, "/v1/projects/prj_dev/operations/"+opID+":cancel", "viewer", "v-can", "{}")
	out := decode(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d code=%v", resp.StatusCode, out["code"])
	}
}

func TestCanonicalHashIgnoresWhitespace(t *testing.T) {
	a := `{ "imageId": "img_seed", "resources": { "cpuMilli": 1, "memoryMiB": 1, "gpu": { "count": 1, "type": "NVIDIA_L4" } } }`
	b := `{"imageId":"img_seed","resources":{"cpuMilli":1,"memoryMiB":1,"gpu":{"count":1,"type":"NVIDIA_L4"}}}`
	h := newHarness(t)
	first := decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "ws", a))
	second := decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "ws", b))
	if first["sandbox"].(map[string]any)["id"] != second["sandbox"].(map[string]any)["id"] {
		t.Fatal("whitespace-equivalent bodies should replay")
	}
}
