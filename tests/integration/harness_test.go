package integration_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ignition.dev/ignition/internal/api"
	"ignition.dev/ignition/internal/auth"
	"ignition.dev/ignition/internal/config"
	"ignition.dev/ignition/internal/controller"
	"ignition.dev/ignition/internal/k8s"
	"ignition.dev/ignition/internal/store"
)

const createBody = `{
	"imageId": "img_seed",
	"resources": {
		"cpuMilli": 1000,
		"memoryMiB": 2048,
		"accelerator": {"count": 1, "type": "NVIDIA_L4"}
	}
}`

// world is one product store shared by the API and the controller, plus a fake cluster.
type world struct {
	ts   *httptest.Server
	mem  *store.Memory
	ctrl *controller.Controller
	fake *k8s.Fake
}

func newWorld(t *testing.T) *world {
	return newWorldWithConfig(t, nil)
}

// newWorldWithConfig builds a world, letting the caller tweak the API config
// (e.g. MaxActiveSandboxes) before the server starts. alice=owner, bob=developer
// and viewer=viewer are seeded on prj_dev with matching static bearer tokens.
func newWorldWithConfig(t *testing.T, mutate func(*config.Config)) *world {
	t.Helper()
	mem := store.NewMemory()
	mem.SeedRole("prj_dev", "alice", auth.RoleOwner)
	mem.SeedRole("prj_dev", "bob", auth.RoleDeveloper)
	mem.SeedRole("prj_dev", "viewer", auth.RoleViewer)
	mem.SeedImage("prj_dev", "img_seed")
	fake := k8s.NewFake()
	ctrl := controller.New(mem, fake, fake, controller.Options{})
	cfg := config.Config{
		EnabledRegion:      "us-central1",
		GatewayURL:         "https://gateway.us-central1.ignition.dev",
		StreamTokenSecret:  "test-stream-secret",
		MaxActiveSandboxes: 10,
		OIDCAudience:       "https://api.ignition.dev",
	}
	if mutate != nil {
		mutate(&cfg)
	}
	srv := api.New(cfg, mem, auth.Static{Tokens: map[string]auth.Principal{
		"alice":  {Subject: "alice"},
		"bob":    {Subject: "bob"},
		"viewer": {Subject: "viewer"},
	}})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &world{ts: ts, mem: mem, ctrl: ctrl, fake: fake}
}

func (w *world) do(t *testing.T, method, path, idemKey, body string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, w.ts.URL+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer alice")
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

func (w *world) createSandbox(t *testing.T, idemKey string) (sandboxID, operationID string) {
	t.Helper()
	resp := w.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", idemKey, createBody)
	body := decode(t, resp)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create status = %d body=%v", resp.StatusCode, body)
	}
	sb := body["sandbox"].(map[string]any)
	op := body["operation"].(map[string]any)
	return sb["id"].(string), op["id"].(string)
}

func (w *world) getSandbox(t *testing.T, id string) map[string]any {
	t.Helper()
	resp := w.do(t, http.MethodGet, "/v1/projects/prj_dev/sandboxes/"+id, "", "")
	body := decode(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get sandbox status = %d body=%v", resp.StatusCode, body)
	}
	return body
}

func (w *world) getOperation(t *testing.T, id string) map[string]any {
	t.Helper()
	resp := w.do(t, http.MethodGet, "/v1/projects/prj_dev/operations/"+id, "", "")
	body := decode(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get operation status = %d body=%v", resp.StatusCode, body)
	}
	return body
}

func (w *world) getProcess(t *testing.T, sandboxID, processID string) map[string]any {
	t.Helper()
	resp := w.do(t, http.MethodGet, "/v1/projects/prj_dev/sandboxes/"+sandboxID+"/processes/"+processID, "", "")
	body := decode(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get process status = %d body=%v", resp.StatusCode, body)
	}
	return body
}

func (w *world) driveToReady(t *testing.T, sandboxID string) {
	t.Helper()
	ctx := context.Background()
	if err := w.ctrl.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	name := k8s.PodName(sandboxID)
	if _, err := w.fake.Get(name); err != nil {
		t.Fatalf("pod after admit: %v", err)
	}
	w.fake.SetScheduled(name, "gke-node-1")
	if err := w.ctrl.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	w.fake.SetRunning(name)
	if err := w.ctrl.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	w.fake.SetReady(name, "GPU-UUID-1")
	if err := w.ctrl.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
}
