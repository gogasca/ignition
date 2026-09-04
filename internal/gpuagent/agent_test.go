package gpuagent

import (
	"context"
	"testing"

	"ignition.dev/ignition/internal/k8s"
)

const goodUUID = "GPU-4a1b2c3d-1122-3344-5566-778899aabbcc"

type fakeInspector struct {
	gpus  []GPU
	procs []ComputeProc
	err   error
}

func (f *fakeInspector) Inventory(context.Context) ([]GPU, error) {
	return f.gpus, f.err
}
func (f *fakeInspector) ComputeProcesses(context.Context) ([]ComputeProc, error) {
	return f.procs, f.err
}

func newNode(t *testing.T, node string, withSandbox bool) *k8s.Fake {
	t.Helper()
	f := k8s.NewFake()
	if withSandbox {
		if err := f.Create(&k8s.Pod{
			Name:     "sbx-1",
			NodeName: node,
			Running:  true,
			Phase:    "Running",
			Labels:   map[string]string{k8s.LabelWorkload: k8s.WorkloadSandbox},
		}); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func TestAttestHealthyGPUStampsPod(t *testing.T) {
	f := newNode(t, "n1", true)
	a := New("n1", f, f, &fakeInspector{gpus: []GPU{{UUID: goodUUID, ECCUncorrected: -1}}})
	if err := a.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	pod, _ := f.Get("sbx-1")
	if pod.Annotations[k8s.AnnotGPUUUID] != goodUUID || pod.Annotations[k8s.AnnotInitHealthy] != "true" {
		t.Fatalf("pod not attested: %v", pod.Annotations)
	}
	if dirty, _ := f.GPUCleanupAmbiguous("n1"); dirty {
		t.Fatal("healthy node marked dirty")
	}
}

func TestAttestResidualProcessMarksNodeAndSkipsPod(t *testing.T) {
	f := newNode(t, "n1", true)
	a := New("n1", f, f, &fakeInspector{
		gpus:  []GPU{{UUID: goodUUID, ECCUncorrected: -1}},
		procs: []ComputeProc{{PID: 4242, UsedMiB: 512}},
	})
	if err := a.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	pod, _ := f.Get("sbx-1")
	if pod.Annotations[k8s.AnnotInitHealthy] == "true" {
		t.Fatal("attested a GPU with residual processes")
	}
	if dirty, _ := f.GPUCleanupAmbiguous("n1"); !dirty {
		t.Fatal("node not marked dirty despite residual process")
	}
}

func TestAttestUnhealthyGPUNotAttested(t *testing.T) {
	f := newNode(t, "n1", true)
	a := New("n1", f, f, &fakeInspector{gpus: []GPU{{UUID: goodUUID, ECCUncorrected: 5}}})
	if err := a.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	pod, _ := f.Get("sbx-1")
	if pod.Annotations[k8s.AnnotInitHealthy] == "true" {
		t.Fatal("attested an unhealthy GPU")
	}
	if dirty, _ := f.GPUCleanupAmbiguous("n1"); !dirty {
		t.Fatal("unhealthy node not marked dirty")
	}
}

func TestAttestRejectsNonCanonicalInspectorUUID(t *testing.T) {
	f := newNode(t, "n1", true)
	a := New("n1", f, f, &fakeInspector{gpus: []GPU{{UUID: "GPU-1", ECCUncorrected: -1}}})
	if err := a.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	pod, _ := f.Get("sbx-1")
	if pod.Annotations[k8s.AnnotGPUUUID] != "" {
		t.Fatalf("stamped a non-canonical UUID: %q", pod.Annotations[k8s.AnnotGPUUUID])
	}
}

func TestAttestIsIdempotent(t *testing.T) {
	f := newNode(t, "n1", true)
	_ = f.PatchAnnotations("sbx-1", map[string]string{
		k8s.AnnotGPUUUID:     goodUUID,
		k8s.AnnotInitHealthy: "true",
	})
	insp := &fakeInspector{err: context.DeadlineExceeded} // must not be consulted
	a := New("n1", f, f, insp)
	if err := a.Reconcile(context.Background()); err != nil {
		t.Fatalf("re-attest of an already-attested pod errored: %v", err)
	}
}

func TestVerifyReuseCleanClearsAnnotation(t *testing.T) {
	f := newNode(t, "n1", false)
	_ = f.MarkNodeGPUCleanup("n1", true) // stale
	a := New("n1", f, f, &fakeInspector{gpus: []GPU{{UUID: goodUUID, ECCUncorrected: -1}}})
	if err := a.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if dirty, _ := f.GPUCleanupAmbiguous("n1"); dirty {
		t.Fatal("clean GPU left the node marked dirty")
	}
}

// TestVerifyReuseCleanClearsReusePendingTaint guards F3: a clean reuse check
// must clear the scheduling-blocking taint, or a torn-down GPU node would
// never come back into the warm pool.
func TestVerifyReuseCleanClearsReusePendingTaint(t *testing.T) {
	f := newNode(t, "n1", false)
	a := New("n1", f, f, &fakeInspector{gpus: []GPU{{UUID: goodUUID, ECCUncorrected: -1}}})
	if err := a.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.GPUReusePending("n1") {
		t.Fatal("clean GPU left the node reuse-pending taint set")
	}
}

// TestVerifyReuseResidualLeavesReusePendingTaint guards the other half of F3:
// a dirty verdict must leave the node blocked from new scheduling (the
// cordon-and-recreate path then replaces it) rather than clear the taint and
// let a new tenant land on an unproven GPU.
func TestVerifyReuseResidualLeavesReusePendingTaint(t *testing.T) {
	f := newNode(t, "n1", true)
	a := New("n1", f, f, &fakeInspector{gpus: []GPU{{UUID: goodUUID, ECCUncorrected: -1}}})
	if err := a.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.Drop("sbx-1")
	a.Inspector = &fakeInspector{
		gpus:  []GPU{{UUID: goodUUID, ECCUncorrected: -1}},
		procs: []ComputeProc{{PID: 9}},
	}
	if err := a.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !f.GPUReusePending("n1") {
		t.Fatal("residual-process verdict cleared the reuse-pending taint")
	}
}

func TestVerifyReuseResidualMarksNode(t *testing.T) {
	f := newNode(t, "n1", true)
	a := New("n1", f, f, &fakeInspector{gpus: []GPU{{UUID: goodUUID, ECCUncorrected: -1}}})
	// pass 1: sandbox present, attest
	if err := a.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	// sandbox leaves, residual process remains
	f.Drop("sbx-1")
	a.Inspector = &fakeInspector{
		gpus:  []GPU{{UUID: goodUUID, ECCUncorrected: -1}},
		procs: []ComputeProc{{PID: 9}},
	}
	if err := a.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if dirty, _ := f.GPUCleanupAmbiguous("n1"); !dirty {
		t.Fatal("residual process after teardown did not mark the node")
	}
}
