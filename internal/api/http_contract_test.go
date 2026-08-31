package api_test

import (
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

func TestImageNotReady(t *testing.T) {
	h := newHarness(t)
	body := `{
		"imageId": "img_unknown",
		"resources": {"cpuMilli": 1, "memoryMiB": 1, "accelerator": {"count": 1, "type": "NVIDIA_L4"}}
	}`
	resp := h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "img", body)
	out := decode(t, resp)
	if resp.StatusCode != http.StatusBadRequest || out["code"] != "IMAGE_NOT_READY" {
		t.Fatalf("status=%d code=%v", resp.StatusCode, out["code"])
	}
}

func TestCreateRejectsInvalidJSON(t *testing.T) {
	h := newHarness(t)
	resp := h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "bad-json", `{`)
	out := decode(t, resp)
	if resp.StatusCode != http.StatusBadRequest || out["code"] != "INVALID_ARGUMENT" {
		t.Fatalf("status=%d code=%v", resp.StatusCode, out["code"])
	}
}

func TestCreateRejectsMissingResources(t *testing.T) {
	h := newHarness(t)
	resp := h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "no-res", `{"imageId":"img_seed"}`)
	out := decode(t, resp)
	if resp.StatusCode != http.StatusBadRequest || out["code"] != "INVALID_ARGUMENT" {
		t.Fatalf("status=%d code=%v", resp.StatusCode, out["code"])
	}
}

func TestCreateIgnoresRemovedSandboxEnvironment(t *testing.T) {
	h := newHarness(t)
	body := `{
		"imageId": "img_seed",
		"environment": {"LOG_LEVEL": "info"},
		"resources": {"cpuMilli": 1, "memoryMiB": 1, "accelerator": {"count": 1, "type": "NVIDIA_L4"}}
	}`
	resp := h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "no-sandbox-env", body)
	out := decode(t, resp)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status=%d body=%v", resp.StatusCode, out)
	}
	sb := out["sandbox"].(map[string]any)
	if _, ok := sb["environment"]; ok {
		t.Fatalf("removed environment leaked into sandbox: %v", sb)
	}
}

func TestCreateAcceptsCPUAccelerator(t *testing.T) {
	h := newHarness(t)
	body := `{
		"imageId": "img_seed",
		"resources": {"cpuMilli": 1, "memoryMiB": 1, "accelerator": {"count": 0, "type": "NONE"}}
	}`
	resp := h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "cpu", body)
	out := decode(t, resp)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status=%d body=%v", resp.StatusCode, out)
	}
	acc := out["sandbox"].(map[string]any)["resources"].(map[string]any)["accelerator"].(map[string]any)
	if acc["type"] != "NONE" {
		t.Fatalf("accelerator = %v", acc)
	}
}

func TestCreateRejectsAcceleratorCountMismatch(t *testing.T) {
	h := newHarness(t)
	body := `{
		"imageId": "img_seed",
		"resources": {"cpuMilli": 1, "memoryMiB": 1, "accelerator": {"count": 1, "type": "NONE"}}
	}`
	resp := h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "cpu-bad", body)
	out := decode(t, resp)
	if resp.StatusCode != http.StatusBadRequest || out["code"] != "INVALID_ARGUMENT" {
		t.Fatalf("status=%d code=%v", resp.StatusCode, out["code"])
	}
}

func TestCreateDefaultsPlacementAndNetwork(t *testing.T) {
	h := newHarness(t)
	body := `{
		"imageId": "img_seed",
		"resources": {"cpuMilli": 1, "memoryMiB": 1, "accelerator": {"count": 1, "type": "NVIDIA_L4"}}
	}`
	out := decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "defaults", body))
	sb := out["sandbox"].(map[string]any)
	if _, ok := sb["generation"]; ok {
		t.Fatalf("internal generation leaked into sandbox: %v", sb)
	}
	pl := sb["placement"].(map[string]any)
	if pl["computeEnvironment"] != "STANDARD" {
		t.Fatalf("placement = %v", pl)
	}
	net := sb["network"].(map[string]any)
	if net["internetAccess"] != "DISABLED" {
		t.Fatalf("network = %v", net)
	}
}

func TestCreateAcceptsEnabledInternetAccess(t *testing.T) {
	h := newHarness(t)
	body := `{
		"imageId": "img_seed",
		"resources": {"cpuMilli": 1, "memoryMiB": 1, "accelerator": {"count": 1, "type": "NVIDIA_L4"}},
		"network": {"internetAccess": "ENABLED"}
	}`
	out := decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "internet", body))
	network := out["sandbox"].(map[string]any)["network"].(map[string]any)
	if network["internetAccess"] != "ENABLED" {
		t.Fatalf("network = %v", network)
	}
}

func TestCreateRejectsInvalidInternetAccess(t *testing.T) {
	h := newHarness(t)
	body := `{
		"imageId": "img_seed",
		"resources": {"cpuMilli": 1, "memoryMiB": 1, "accelerator": {"count": 1, "type": "NVIDIA_L4"}},
		"network": {"internetAccess": "ALLOW_LIST"}
	}`
	resp := h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "bad-network", body)
	out := decode(t, resp)
	if resp.StatusCode != http.StatusBadRequest || out["code"] != "INVALID_ARGUMENT" {
		t.Fatalf("status=%d body=%v", resp.StatusCode, out)
	}
}

func TestCreateAcceptsBareMetalEnvironment(t *testing.T) {
	h := newHarness(t)
	body := `{
		"imageId": "img_seed",
		"resources": {"cpuMilli": 1, "memoryMiB": 1, "accelerator": {"count": 1, "type": "NVIDIA_L4"}},
		"placement": {"computeEnvironment": "BARE_METAL"}
	}`
	out := decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "bare-metal", body))
	pl := out["sandbox"].(map[string]any)["placement"].(map[string]any)
	if pl["computeEnvironment"] != "BARE_METAL" {
		t.Fatalf("placement = %v", pl)
	}
}

func TestCreateRejectsInvalidComputeEnvironment(t *testing.T) {
	h := newHarness(t)
	body := `{
		"imageId": "img_seed",
		"resources": {"cpuMilli": 1, "memoryMiB": 1, "accelerator": {"count": 1, "type": "NVIDIA_L4"}},
		"placement": {"computeEnvironment": "AUTOMATIC"}
	}`
	resp := h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "environment", body)
	out := decode(t, resp)
	if resp.StatusCode != http.StatusBadRequest || out["code"] != "INVALID_ARGUMENT" {
		t.Fatalf("status=%d code=%v", resp.StatusCode, out["code"])
	}
}

func TestEmptySandboxList(t *testing.T) {
	h := newHarness(t)
	out := decode(t, h.do(t, http.MethodGet, "/v1/projects/prj_dev/sandboxes", "alice", "", ""))
	items, _ := out["sandboxes"].([]any)
	if items == nil {
		t.Fatal("sandboxes must be [] not null")
	}
	if len(items) != 0 {
		t.Fatalf("len = %d", len(items))
	}
}

func TestRequestIDEcho(t *testing.T) {
	h := newHarness(t)
	req, err := http.NewRequest(http.MethodGet, h.ts.URL+"/v1/projects/prj_dev/sandboxes", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer alice")
	req.Header.Set("X-Request-Id", "req_client")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("X-Request-Id"); got != "req_client" {
		t.Fatalf("X-Request-Id = %q", got)
	}
}

func TestUnknownCustomMethodIs404(t *testing.T) {
	h := newHarness(t)
	created := decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "custom", createBody))
	id := created["sandbox"].(map[string]any)["id"].(string)
	resp := h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes/"+id+":explode", "alice", "x", "{}")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "application/json") {
		t.Fatalf("unknown custom method returned JSON Status, content-type=%q", ct)
	}
}

func TestTerminateMissingSandboxIs404(t *testing.T) {
	h := newHarness(t)
	resp := h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes/sbx_missing0000000001:terminate", "alice", "t-miss", "{}")
	out := decode(t, resp)
	if resp.StatusCode != http.StatusNotFound || out["code"] != "NOT_FOUND" {
		t.Fatalf("status=%d code=%v", resp.StatusCode, out["code"])
	}
}

func TestTerminateIdempotentReplay(t *testing.T) {
	h := newHarness(t)
	created := decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "term-id", createBody))
	id := created["sandbox"].(map[string]any)["id"].(string)
	a := decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes/"+id+":terminate", "alice", "same-term", "{}"))
	b := decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes/"+id+":terminate", "alice", "same-term", "{}"))
	if a["operation"].(map[string]any)["id"] != b["operation"].(map[string]any)["id"] {
		t.Fatalf("terminate replay diverged: %v vs %v", a, b)
	}
}

func TestListProcessesMissingSandboxIsEmpty(t *testing.T) {
	// Spec wants 404. Memory lists by sandbox id without an existence check.
	h := newHarness(t)
	resp := h.do(t, http.MethodGet, "/v1/projects/prj_dev/sandboxes/sbx_nope00000000000001/processes", "alice", "", "")
	out := decode(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%v", resp.StatusCode, out)
	}
	items, _ := out["processes"].([]any)
	if len(items) != 0 {
		t.Fatalf("processes = %v", items)
	}
}

func TestProcessOnMissingSandboxIs404(t *testing.T) {
	h := newHarness(t)
	resp := h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes/sbx_nope00000000000001/processes", "alice", "p-miss", `{"command":["true"]}`)
	out := decode(t, resp)
	if resp.StatusCode != http.StatusNotFound || out["code"] != "NOT_FOUND" {
		t.Fatalf("status=%d code=%v", resp.StatusCode, out["code"])
	}
}

func TestProcessCreateIdempotentReplay(t *testing.T) {
	h := newHarness(t)
	created := decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "prc-idem", createBody))
	sbx := created["sandbox"].(map[string]any)["id"].(string)
	h.mem.SetSandboxState("prj_dev", sbx, "READY")
	a := decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes/"+sbx+"/processes", "alice", "same-p", `{"command":["true"]}`))
	b := decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes/"+sbx+"/processes", "alice", "same-p", `{"command":["true"]}`))
	if a["id"] != b["id"] {
		t.Fatalf("process replay diverged %v vs %v", a["id"], b["id"])
	}
}

func TestProcessRejectsEmptyCommand(t *testing.T) {
	h := newHarness(t)
	created := decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "prc-cmd", createBody))
	sbx := created["sandbox"].(map[string]any)["id"].(string)
	h.mem.SetSandboxState("prj_dev", sbx, "READY")
	resp := h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes/"+sbx+"/processes", "alice", "p-empty", `{"command":[]}`)
	out := decode(t, resp)
	if resp.StatusCode != http.StatusBadRequest || out["code"] != "INVALID_ARGUMENT" {
		t.Fatalf("status=%d code=%v", resp.StatusCode, out["code"])
	}
}

func TestGetUnknownProcessIs404(t *testing.T) {
	h := newHarness(t)
	created := decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "prc-404", createBody))
	sbx := created["sandbox"].(map[string]any)["id"].(string)
	resp := h.do(t, http.MethodGet, "/v1/projects/prj_dev/sandboxes/"+sbx+"/processes/prc_nope0000000000001", "alice", "", "")
	out := decode(t, resp)
	if resp.StatusCode != http.StatusNotFound || out["code"] != "NOT_FOUND" {
		t.Fatalf("status=%d code=%v", resp.StatusCode, out["code"])
	}
}

func TestViewerCannotExec(t *testing.T) {
	h := newHarness(t)
	created := decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "prc-view", createBody))
	sbx := created["sandbox"].(map[string]any)["id"].(string)
	h.mem.SetSandboxState("prj_dev", sbx, "READY")
	resp := h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes/"+sbx+"/processes", "viewer", "p-v", `{"command":["true"]}`)
	out := decode(t, resp)
	if resp.StatusCode != http.StatusForbidden || out["code"] != "PERMISSION_DENIED" {
		t.Fatalf("status=%d code=%v", resp.StatusCode, out["code"])
	}
}

func TestAttachSameIdempotencyKeyReplaysEpoch(t *testing.T) {
	h := newHarness(t)
	created := decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "att-idem", createBody))
	sbx := created["sandbox"].(map[string]any)["id"].(string)
	h.mem.SetSandboxState("prj_dev", sbx, "READY")
	proc := decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes/"+sbx+"/processes", "alice", "p-att-i", `{"command":["true"]}`))
	prc := proc["id"].(string)
	a := decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes/"+sbx+"/processes/"+prc+":attach", "alice", "same-att", "{}"))
	time.Sleep(time.Millisecond)
	b := decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes/"+sbx+"/processes/"+prc+":attach", "alice", "same-att", "{}"))
	if a["streamEpoch"] != b["streamEpoch"] {
		t.Fatalf("attach should replay the same epoch, got %v then %v", a["streamEpoch"], b["streamEpoch"])
	}
}

func TestSignalMissingProcessIs404(t *testing.T) {
	h := newHarness(t)
	created := decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "sig-404", createBody))
	sbx := created["sandbox"].(map[string]any)["id"].(string)
	h.mem.SetSandboxState("prj_dev", sbx, "READY")
	resp := h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes/"+sbx+"/processes/prc_nope0000000000001:signal", "alice", "sig", `{"signal":"SIGTERM"}`)
	out := decode(t, resp)
	if resp.StatusCode != http.StatusNotFound || out["code"] != "NOT_FOUND" {
		t.Fatalf("status=%d code=%v", resp.StatusCode, out["code"])
	}
}

func TestWatchOperationSSE(t *testing.T) {
	h := newHarness(t)
	created := decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "op-watch", createBody))
	opID := created["operation"].(map[string]any)["id"].(string)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.ts.URL+"/v1/projects/prj_dev/operations/"+opID+":watch", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer alice")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "event: snapshot") {
		t.Fatalf("watch body = %s", raw)
	}
}

func TestCancelCreateFailsSandbox(t *testing.T) {
	h := newHarness(t)
	created := decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "op-leave", createBody))
	opID := created["operation"].(map[string]any)["id"].(string)
	sbx := created["sandbox"].(map[string]any)["id"].(string)
	_ = decode(t, h.do(t, http.MethodPost, "/v1/projects/prj_dev/operations/"+opID+":cancel", "alice", "op-can-2", "{}"))
	got := decode(t, h.do(t, http.MethodGet, "/v1/projects/prj_dev/sandboxes/"+sbx, "alice", "", ""))
	if got["state"] != "FAILED" || got["stateReason"] != "CANCELLED" {
		t.Fatalf("sandbox after cancel = %v %v", got["state"], got["stateReason"])
	}
}

func TestGetUnknownOperationIs404(t *testing.T) {
	h := newHarness(t)
	resp := h.do(t, http.MethodGet, "/v1/projects/prj_dev/operations/op_nope000000000000001", "alice", "", "")
	out := decode(t, resp)
	if resp.StatusCode != http.StatusNotFound || out["code"] != "NOT_FOUND" {
		t.Fatalf("status=%d code=%v", resp.StatusCode, out["code"])
	}
}

func TestJWTAccessTokenAccepted(t *testing.T) {
	mem := store.NewMemory()
	mem.SeedRole("prj_dev", "jwt-user", auth.RoleOwner)
	mem.SeedImage("prj_dev", "img_seed")
	secret := []byte("hmac-api-secret")
	srv := api.New(config.Config{
		EnabledRegion:      "us-central1",
		StreamTokenSecret:  "stream-secret",
		MaxActiveSandboxes: 2,
		OIDCAudience:       "https://api.ignition.dev",
	}, mem, &auth.JWT{
		Issuer:    "https://issuer.example",
		Audience:  "https://api.ignition.dev",
		HMAC:      secret,
		Algorithm: "HS256",
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss": "https://issuer.example",
		"aud": "https://api.ignition.dev",
		"sub": "jwt-user",
		"iat": now.Unix(),
		"nbf": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	})
	tok.Header["typ"] = "at+jwt"
	raw, err := tok.SignedString(secret)
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/projects/prj_dev/sandboxes", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+raw)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
}

func TestStreamTokenRejectedAsAPIBearer(t *testing.T) {
	mem := store.NewMemory()
	mem.SeedRole("prj_dev", "alice", auth.RoleOwner)
	secret := []byte("shared-secret")
	srv := api.New(config.Config{
		EnabledRegion: "us-central1",
		OIDCAudience:  "https://api.ignition.dev",
	}, mem, &auth.JWT{
		Issuer:    "https://issuer.example",
		Audience:  "https://api.ignition.dev",
		HMAC:      secret,
		Algorithm: "HS256",
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss": "https://issuer.example",
		"aud": "https://api.ignition.dev",
		"sub": "alice",
		"iat": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	})
	tok.Header["typ"] = "stream+jwt"
	raw, err := tok.SignedString(secret)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/projects/prj_dev/sandboxes", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+raw)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusUnauthorized || out["code"] != "UNAUTHENTICATED" {
		t.Fatalf("status=%d code=%v", resp.StatusCode, out["code"])
	}
}
