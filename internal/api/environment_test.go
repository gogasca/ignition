package api_test

import (
	"fmt"
	"net/http"
	"testing"
)

// CreateSandbox's plain (non-secret) environment round-trips through create,
// get, and list — it is a resolved part of the sandbox, like command/args.
func TestCreateSandboxEnvironmentRoundTrips(t *testing.T) {
	h := newHarness(t)
	body := `{"imageId":"img_seed","environment":{"TASK_ID":"task_0001","MAX_TURNS":"12"}}`
	resp := h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "idem-env", body)
	got := decode(t, resp)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d body=%v", resp.StatusCode, got)
	}
	sb := got["sandbox"].(map[string]any)
	env, _ := sb["environment"].(map[string]any)
	if env["TASK_ID"] != "task_0001" || env["MAX_TURNS"] != "12" {
		t.Fatalf("create response environment = %v", env)
	}
	sbID := sb["id"].(string)

	gresp := h.do(t, http.MethodGet, "/v1/projects/prj_dev/sandboxes/"+sbID, "alice", "", "")
	gotGet := decode(t, gresp)
	genv, _ := gotGet["environment"].(map[string]any)
	if genv["TASK_ID"] != "task_0001" {
		t.Fatalf("GetSandbox environment = %v", genv)
	}
}

// The IGNITION_ namespace is reserved for the controller's own Pod env
// (sandbox id, project id, accelerator) — a client can never override it via
// the environment field, and the rejection is loud, not a silent drop.
func TestCreateSandboxRejectsReservedEnvironmentKey(t *testing.T) {
	h := newHarness(t)
	body := `{"imageId":"img_seed","environment":{"IGNITION_ACCELERATOR":"evil"}}`
	resp := h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "idem-reserved", body)
	got := decode(t, resp)
	if resp.StatusCode != http.StatusBadRequest || got["code"] != "INVALID_ARGUMENT" {
		t.Fatalf("status = %d body=%v", resp.StatusCode, got)
	}
}

func TestCreateSandboxRejectsInvalidEnvironmentKeyName(t *testing.T) {
	h := newHarness(t)
	body := `{"imageId":"img_seed","environment":{"not a valid name!":"x"}}`
	resp := h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "idem-badname", body)
	got := decode(t, resp)
	if resp.StatusCode != http.StatusBadRequest || got["code"] != "INVALID_ARGUMENT" {
		t.Fatalf("status = %d body=%v", resp.StatusCode, got)
	}
}

func TestCreateSandboxRejectsTooManyEnvironmentVars(t *testing.T) {
	h := newHarness(t)
	env := "{"
	for i := 0; i < 40; i++ {
		if i > 0 {
			env += ","
		}
		env += fmt.Sprintf(`"VAR_%d":"x"`, i)
	}
	env += "}"
	body := `{"imageId":"img_seed","environment":` + env + `}`
	resp := h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "idem-toomany", body)
	got := decode(t, resp)
	if resp.StatusCode != http.StatusBadRequest || got["code"] != "INVALID_ARGUMENT" {
		t.Fatalf("status = %d body=%v", resp.StatusCode, got)
	}
}

// A sandbox created with no environment omits the key entirely rather than
// returning an empty object — consistent with command/args/labels.
func TestCreateSandboxOmitsEmptyEnvironment(t *testing.T) {
	h := newHarness(t)
	body := `{"imageId":"img_seed"}`
	resp := h.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes", "alice", "idem-noenv", body)
	got := decode(t, resp)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d body=%v", resp.StatusCode, got)
	}
	if _, ok := got["sandbox"].(map[string]any)["environment"]; ok {
		t.Fatalf("environment should be omitted when unset, got %v", got["sandbox"])
	}
}
