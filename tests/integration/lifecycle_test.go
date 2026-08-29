package integration_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"ignition.dev/ignition/internal/k8s"
)

func TestCreateThroughReadyProcessAttachAndTerminate(t *testing.T) {
	w := newWorld(t)
	sbx, opID := w.createSandbox(t, "life-1")

	got := w.getSandbox(t, sbx)
	if got["state"] != "CREATING" {
		t.Fatalf("after admit state = %v", got["state"])
	}

	w.driveToReady(t, sbx)

	ready := w.getSandbox(t, sbx)
	if ready["state"] != "READY" {
		t.Fatalf("after observe state = %v", ready["state"])
	}
	if ready["stateReason"] != "READY" {
		t.Fatalf("stateReason = %v", ready["stateReason"])
	}
	op := w.getOperation(t, opID)
	if op["state"] != "SUCCEEDED" {
		t.Fatalf("operation state = %v", op["state"])
	}

	procBody := decode(t, w.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes/"+sbx+"/processes", "p-1", `{"command":["sleep","1"]}`))
	if procBody["state"] != "CREATING" {
		t.Fatalf("process after create = %v", procBody["state"])
	}
	prc := procBody["id"].(string)

	if w.getProcess(t, sbx, prc)["state"] != "CREATING" {
		t.Fatalf("process after reconcile without init = %v", w.getProcess(t, sbx, prc)["state"])
	}
	w.fake.SetProcessObserved(k8s.PodName(sbx), prc, "RUNNING", nil)
	if err := w.ctrl.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.getProcess(t, sbx, prc)["state"] != "RUNNING" {
		t.Fatalf("process after init observe = %v", w.getProcess(t, sbx, prc)["state"])
	}

	att := decode(t, w.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes/"+sbx+"/processes/"+prc+":attach", "att-1", "{}"))
	if att["streamToken"] == "" || att["gatewayUrl"] == "" {
		t.Fatalf("attach = %v", att)
	}

	term := decode(t, w.do(t, http.MethodPost, "/v1/projects/prj_dev/sandboxes/"+sbx+":terminate", "term-1", "{}"))
	if term["sandbox"].(map[string]any)["state"] != "TERMINATING" {
		t.Fatalf("terminate = %v", term)
	}

	if err := w.ctrl.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.fake.Count() != 0 {
		t.Fatal("pod should be deleted on TERMINATING")
	}
	if err := w.ctrl.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.getSandbox(t, sbx)["state"] != "FINISHED" {
		t.Fatalf("after delete observe state = %v", w.getSandbox(t, sbx)["state"])
	}
}

func TestGETReadsStoreNotCluster(t *testing.T) {
	w := newWorld(t)
	sbx, _ := w.createSandbox(t, "sql-1")
	if err := w.ctrl.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := w.fake.Get(k8s.PodName(sbx)); err != nil {
		t.Fatalf("controller should have created a pod: %v", err)
	}
	got := w.getSandbox(t, sbx)
	if got["state"] != "CREATING" {
		t.Fatalf("GET must stay CREATING until UpdateObserved, got %v", got["state"])
	}
}

func TestTwoSandboxesIndependentPods(t *testing.T) {
	w := newWorld(t)
	a, _ := w.createSandbox(t, "a")
	b, _ := w.createSandbox(t, "b")
	w.driveToReady(t, a)
	if w.getSandbox(t, b)["state"] != "CREATING" {
		t.Fatalf("sandbox b should still be CREATING, got %v", w.getSandbox(t, b)["state"])
	}
	if _, err := w.fake.Get(k8s.PodName(b)); err != nil {
		t.Fatalf("pod b should exist after driveToReady of a (shared reconcile): %v", err)
	}
	w.fake.SetReady(k8s.PodName(b), "GPU-UUID-2")
	if err := w.ctrl.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.getSandbox(t, a)["state"] != "READY" || w.getSandbox(t, b)["state"] != "READY" {
		t.Fatalf("a=%v b=%v", w.getSandbox(t, a)["state"], w.getSandbox(t, b)["state"])
	}
}

func TestWorkerLostSurfacesOnGET(t *testing.T) {
	w := newWorld(t)
	sbx, opID := w.createSandbox(t, "lost-1")
	w.driveToReady(t, sbx)
	w.fake.Drop(k8s.PodName(sbx))
	if err := w.ctrl.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := w.getSandbox(t, sbx)
	if got["state"] != "FAILED" || got["stateReason"] != "WORKER_LOST" {
		t.Fatalf("got %v %v", got["state"], got["stateReason"])
	}
	if w.getOperation(t, opID)["state"] != "FAILED" {
		t.Fatalf("operation = %v", w.getOperation(t, opID)["state"])
	}
}

func TestCancelCreateDoesNotCreatePod(t *testing.T) {
	w := newWorld(t)
	sbx, opID := w.createSandbox(t, "cancel-1")
	cancelled := decode(t, w.do(t, http.MethodPost, "/v1/projects/prj_dev/operations/"+opID+":cancel", "op-can", "{}"))
	if cancelled["state"] != "CANCELLED" {
		t.Fatalf("cancel = %v", cancelled)
	}
	if err := w.ctrl.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := w.fake.Get(k8s.PodName(sbx)); !errors.Is(err, k8s.ErrNotFound) {
		t.Fatalf("pod after cancelled create: %v", err)
	}
	got := w.getSandbox(t, sbx)
	if got["state"] != "FAILED" || got["stateReason"] != "CANCELLED" {
		t.Fatalf("sandbox after cancel+reconcile = %v %v", got["state"], got["stateReason"])
	}
}
