package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"ignition.dev/ignition/internal/store"
)

func mainProcs(t *testing.T, h *harness, sbID string) []store.Process {
	t.Helper()
	p, err := h.mem.ListProcessesBySandbox(context.Background(), "prj_dev", sbID)
	if err != nil {
		t.Fatalf("list processes: %v", err)
	}
	return p
}

// Managed sandbox: a provided command/args becomes the sandbox's main
// supervised process — a real processes row, created in the same request.
func TestCreateSandboxManagedCommandSeedsMainProcess(t *testing.T) {
	h := newHarness(t)
	body := `{"imageId":"img_seed","command":["python","-m","http.server"],"args":["9000"]}`
	resp := h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "idem-mc", body)
	got := decode(t, resp)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d body=%v", resp.StatusCode, got)
	}
	sbID := got["sandbox"].(map[string]any)["id"].(string)

	procs := mainProcs(t, h, sbID)
	if len(procs) != 1 {
		t.Fatalf("want 1 main process, got %d: %+v", len(procs), procs)
	}
	want := []string{"python", "-m", "http.server", "9000"}
	if !equalStrs(procs[0].Command, want) {
		t.Fatalf("main process command = %v, want %v", procs[0].Command, want)
	}

	// And it is surfaced through the public process list.
	lresp := h.do(t, http.MethodGet, "/v1/projects/prj_dev/sandboxes/"+sbID+"/processes", "alice", "", "")
	var list struct {
		Processes []map[string]any `json:"processes"`
	}
	defer lresp.Body.Close()
	if err := json.NewDecoder(lresp.Body).Decode(&list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Processes) != 1 {
		t.Fatalf("process list len = %d", len(list.Processes))
	}
}

// Native sandbox: command/args are the container spec, NOT a supervised
// process — no processes row is created.
func TestCreateSandboxNativeCommandDoesNotSeedProcess(t *testing.T) {
	h := newHarness(t)
	body := `{"imageId":"img_seed","nativeEntrypoint":true,"command":["/bin/app"],"args":["--flag"]}`
	resp := h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "idem-nc", body)
	got := decode(t, resp)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d body=%v", resp.StatusCode, got)
	}
	sbID := got["sandbox"].(map[string]any)["id"].(string)
	if procs := mainProcs(t, h, sbID); len(procs) != 0 {
		t.Fatalf("native sandbox must not seed a process, got %+v", procs)
	}
}

// No command: managed sandbox seeds nothing; the supervisor idles for exec.
func TestCreateSandboxNoCommandSeedsNoProcess(t *testing.T) {
	h := newHarness(t)
	resp := h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "idem-nn", `{"imageId":"img_seed"}`)
	got := decode(t, resp)
	sbID := got["sandbox"].(map[string]any)["id"].(string)
	if procs := mainProcs(t, h, sbID); len(procs) != 0 {
		t.Fatalf("want no process, got %+v", procs)
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
