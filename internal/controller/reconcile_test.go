package controller_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"ignition.dev/ignition/internal/adminz"
	"ignition.dev/ignition/internal/controller"
	"ignition.dev/ignition/internal/k8s"
	"ignition.dev/ignition/internal/store"
)

func admit(t *testing.T, m *store.Memory, timeouts store.TimeoutSpec) store.CreateSandboxResult {
	t.Helper()
	m.SeedImage("prj_dev", "img_seed")
	if timeouts.StartupSeconds == 0 {
		timeouts.StartupSeconds = 120
	}
	res, err := m.CreateSandbox(context.Background(), store.CreateSandboxInput{
		ProjectID: "prj_dev",
		Principal: "alice",
		IdemKey:   t.Name(),
		IdemHash:  t.Name(),
		ImageID:   "img_seed",
		Command:   []string{"sleep", "1"},
		Resources: store.ResourceSpec{CPUMilli: 1000, MemoryMiB: 2048, Accelerator: store.AcceleratorSpec{Count: 1, Type: store.AcceleratorNVIDIAL4}},
		Timeouts:  timeouts,
		MaxActive: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func mustGet(t *testing.T, m *store.Memory, id string) store.Sandbox {
	t.Helper()
	sb, err := m.GetSandbox(context.Background(), "prj_dev", id)
	if err != nil {
		t.Fatal(err)
	}
	return sb
}

func TestReconcileCreatesDeterministicPod(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	c := controller.New(m, fake, fake, controller.Options{})
	res := admit(t, m, store.TimeoutSpec{})
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	name := k8s.PodName(res.Sandbox.ID)
	p, err := fake.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	if p.Spec.RuntimeClassName != k8s.RuntimeClass {
		t.Fatalf("runtime = %q", p.Spec.RuntimeClassName)
	}
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.Creates != 1 {
		t.Fatalf("duplicate create: %d", fake.Creates)
	}
}

func TestBareMetalFailsWithoutFallingBackToGKE(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	c := controller.New(m, fake, fake, controller.Options{})
	m.SeedImage("prj_dev", "img_seed")
	res, err := m.CreateSandbox(context.Background(), store.CreateSandboxInput{
		ProjectID: "prj_dev",
		Principal: "alice",
		IdemKey:   t.Name(),
		IdemHash:  t.Name(),
		ImageID:   "img_seed",
		Resources: store.ResourceSpec{CPUMilli: 1000, MemoryMiB: 2048, Accelerator: store.AcceleratorSpec{Count: 1, Type: store.AcceleratorNVIDIAL4}},
		Placement: store.PlacementSpec{ComputeEnvironment: store.ComputeEnvironmentBareMetal},
		Timeouts:  store.TimeoutSpec{StartupSeconds: 120},
		MaxActive: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.Creates != 0 {
		t.Fatalf("bare-metal request created %d GKE Pods", fake.Creates)
	}
	sb := mustGet(t, m, res.Sandbox.ID)
	if sb.State != "FAILED" || sb.StateReason != "COMPUTE_ENVIRONMENT_UNAVAILABLE" {
		t.Fatalf("state=%s reason=%s", sb.State, sb.StateReason)
	}
}

func TestCPUSandboxSchedulesAndReachesReady(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	c := controller.New(m, fake, fake, controller.Options{})
	m.SeedImage("prj_dev", "img_seed")
	res, err := m.CreateSandbox(context.Background(), store.CreateSandboxInput{
		ProjectID: "prj_dev",
		Principal: "alice",
		IdemKey:   t.Name(),
		IdemHash:  t.Name(),
		ImageID:   "img_seed",
		Resources: store.ResourceSpec{CPUMilli: 1000, MemoryMiB: 2048, Accelerator: store.AcceleratorSpec{Type: store.AcceleratorNone}},
		Timeouts:  store.TimeoutSpec{StartupSeconds: 120},
		MaxActive: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	name := k8s.PodName(res.Sandbox.ID)
	pod, err := fake.Get(name)
	if err != nil {
		t.Fatalf("CPU sandbox created no Pod: %v", err)
	}
	if pod.Spec.NodeSelector[k8s.NodePoolLabel] != k8s.CPUNodePoolValue {
		t.Fatalf("node selector = %v", pod.Spec.NodeSelector)
	}
	if pod.Spec.Containers[0].GPU != "" {
		t.Fatalf("CPU sandbox requested a GPU: %q", pod.Spec.Containers[0].GPU)
	}
	if pod.Spec.AntiAffinityHostname {
		t.Fatal("CPU sandbox should not require one-per-node anti-affinity")
	}
	if pod.Spec.Containers[0].Env[k8s.EnvAccelerator] != store.AcceleratorNone {
		t.Fatalf("IGNITION_ACCELERATOR = %q", pod.Spec.Containers[0].Env[k8s.EnvAccelerator])
	}

	fake.SetReady(name, "")
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if sb := mustGet(t, m, res.Sandbox.ID); sb.State != "READY" {
		t.Fatalf("state = %s", sb.State)
	}
}

func TestUnknownAcceleratorFailsClosed(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	c := controller.New(m, fake, fake, controller.Options{})
	m.SeedImage("prj_dev", "img_seed")
	res, err := m.CreateSandbox(context.Background(), store.CreateSandboxInput{
		ProjectID: "prj_dev", Principal: "alice", IdemKey: t.Name(), IdemHash: t.Name(),
		ImageID:   "img_seed",
		Resources: store.ResourceSpec{CPUMilli: 1000, MemoryMiB: 2048, Accelerator: store.AcceleratorSpec{Type: "TPU_V5E"}},
		Timeouts:  store.TimeoutSpec{StartupSeconds: 120},
		MaxActive: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.Creates != 0 {
		t.Fatalf("unknown accelerator created %d Pods", fake.Creates)
	}
	if sb := mustGet(t, m, res.Sandbox.ID); sb.State != "FAILED" || sb.StateReason != "WORKLOAD_NOT_SUPPORTED" {
		t.Fatalf("state=%s reason=%s", sb.State, sb.StateReason)
	}
}

func TestReconcileAlreadyExistsIsSuccess(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	c := controller.New(m, fake, fake, controller.Options{})
	res := admit(t, m, store.TimeoutSpec{})
	spec := k8s.SandboxPod(res.Sandbox, "img")
	if err := fake.Create(spec); err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.Creates != 1 {
		t.Fatalf("creates = %d", fake.Creates)
	}
}

func TestReconcileStateMachine(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	c := controller.New(m, fake, fake, controller.Options{})
	res := admit(t, m, store.TimeoutSpec{})
	id := res.Sandbox.ID
	name := k8s.PodName(id)
	ctx := context.Background()

	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if mustGet(t, m, id).State != "CREATING" {
		t.Fatal("stay CREATING until scheduled")
	}

	fake.SetScheduled(name, "gke-node-1")
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if mustGet(t, m, id).State != "SCHEDULED" {
		t.Fatalf("got %s", mustGet(t, m, id).State)
	}

	fake.SetRunning(name)
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if mustGet(t, m, id).State != "STARTED" {
		t.Fatalf("got %s", mustGet(t, m, id).State)
	}

	fake.SetReady(name, "GPU-UUID-1")
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	sb := mustGet(t, m, id)
	if sb.State != "READY" {
		t.Fatalf("got %s", sb.State)
	}
	op, err := m.GetOperation(ctx, "prj_dev", sb.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if op.State != "SUCCEEDED" {
		t.Fatalf("operation = %s", op.State)
	}
}

func TestWorkerLost(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	c := controller.New(m, fake, fake, controller.Options{})
	res := admit(t, m, store.TimeoutSpec{})
	ctx := context.Background()
	_ = c.Reconcile(ctx)
	name := k8s.PodName(res.Sandbox.ID)
	fake.SetReady(name, "GPU-1")
	_ = c.Reconcile(ctx)
	if mustGet(t, m, res.Sandbox.ID).State != "READY" {
		t.Fatal(mustGet(t, m, res.Sandbox.ID).State)
	}
	before := m.QuotaActive("prj_dev")
	fake.Drop(name)
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	sb := mustGet(t, m, res.Sandbox.ID)
	if sb.State != "FAILED" || sb.StateReason != "WORKER_LOST" {
		t.Fatalf("%s %s", sb.State, sb.StateReason)
	}
	if m.QuotaActive("prj_dev") >= before {
		t.Fatal("quota not released")
	}
}

func TestTerminateDeletesPodThenFinished(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	c := controller.New(m, fake, fake, controller.Options{})
	res := admit(t, m, store.TimeoutSpec{})
	ctx := context.Background()
	_ = c.Reconcile(ctx)
	name := k8s.PodName(res.Sandbox.ID)
	fake.SetReady(name, "GPU-1")
	_ = c.Reconcile(ctx)
	if _, err := m.TerminateSandbox(ctx, "prj_dev", res.Sandbox.ID, "alice", "t", "th", "tr"); err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if fake.Count() != 0 {
		t.Fatal("pod should be deleted")
	}
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if mustGet(t, m, res.Sandbox.ID).State != "FINISHED" {
		t.Fatalf("got %s", mustGet(t, m, res.Sandbox.ID).State)
	}
}

func TestStartupTimeoutUnschedulable(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	c := controller.New(m, fake, fake, controller.Options{Now: func() time.Time { return now }})
	m.SeedSandbox(store.Sandbox{
		ID:         "sbx_timeout0000000001",
		ProjectID:  "prj_dev",
		State:      "CREATING",
		ImageID:    "img_seed",
		CreateTime: now.Add(-2 * time.Minute),
		Timeouts:   store.TimeoutSpec{StartupSeconds: 30},
		Resources:  store.ResourceSpec{CPUMilli: 1, MemoryMiB: 1},
	})
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	sb := mustGet(t, m, "sbx_timeout0000000001")
	if sb.State != "FAILED" || sb.StateReason != "CAPACITY_UNAVAILABLE" {
		t.Fatalf("%s %s", sb.State, sb.StateReason)
	}
	if fake.Creates != 0 {
		t.Fatal("must not create after deadline")
	}
}

func TestImagePullFailure(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	c := controller.New(m, fake, fake, controller.Options{})
	res := admit(t, m, store.TimeoutSpec{})
	ctx := context.Background()
	_ = c.Reconcile(ctx)
	fake.SetFailed(k8s.PodName(res.Sandbox.ID), "ErrImagePull")
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	sb := mustGet(t, m, res.Sandbox.ID)
	if sb.StateReason != "IMAGE_UNAVAILABLE" {
		t.Fatalf("reason = %s", sb.StateReason)
	}
}

func TestAmbiguousGPUCordon(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	c := controller.New(m, fake, fake, controller.Options{})
	res := admit(t, m, store.TimeoutSpec{})
	ctx := context.Background()
	_ = c.Reconcile(ctx)
	name := k8s.PodName(res.Sandbox.ID)
	fake.SetScheduled(name, "gke-node-1")
	fake.SetReady(name, "GPU-1")
	_ = c.Reconcile(ctx)
	if _, err := m.TerminateSandbox(ctx, "prj_dev", res.Sandbox.ID, "alice", "t", "h", "tr"); err != nil {
		t.Fatal(err)
	}
	fake.SetDeleting(name, true)
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fake.Nodes) != 1 || fake.Nodes[0] != "gke-node-1" {
		t.Fatalf("cordon calls = %v", fake.Nodes)
	}
}

// The authoritative dirty signal is the Node annotation ignition-gpu-agent
// writes after a failed reuse check — not a Pod annotation.
func TestDirtyNodeCordonOnTeardown(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	c := controller.New(m, fake, fake, controller.Options{})
	res := admit(t, m, store.TimeoutSpec{})
	ctx := context.Background()
	_ = c.Reconcile(ctx)
	name := k8s.PodName(res.Sandbox.ID)
	fake.SetScheduled(name, "gke-node-7")
	fake.SetReady(name, k8s.FakeGPUUUID)
	_ = c.Reconcile(ctx)

	fake.MarkNodeDirty("gke-node-7") // agent's verdict
	if _, err := m.TerminateSandbox(ctx, "prj_dev", res.Sandbox.ID, "alice", "t", "h", "tr"); err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fake.Nodes) != 1 || fake.Nodes[0] != "gke-node-7" {
		t.Fatalf("expected gke-node-7 cordoned, got %v", fake.Nodes)
	}
}

// A clean node returns to the warm pool untouched.
func TestCleanNodeNotCordonedOnTeardown(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	c := controller.New(m, fake, fake, controller.Options{})
	res := admit(t, m, store.TimeoutSpec{})
	ctx := context.Background()
	_ = c.Reconcile(ctx)
	name := k8s.PodName(res.Sandbox.ID)
	fake.SetScheduled(name, "gke-node-8")
	fake.SetReady(name, k8s.FakeGPUUUID)
	_ = c.Reconcile(ctx)
	if _, err := m.TerminateSandbox(ctx, "prj_dev", res.Sandbox.ID, "alice", "t", "h", "tr"); err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fake.Nodes) != 0 {
		t.Fatalf("clean node was cordoned: %v", fake.Nodes)
	}
}

// TestGPUTeardownTaintsNodeReusePending guards F3: the controller must block
// new scheduling on a GPU sandbox's node the moment it deletes that sandbox's
// Pod, not wait for ignition-gpu-agent's next tick, since the container ran
// with the GPU bound before any attestation happened.
func TestGPUTeardownTaintsNodeReusePending(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	c := controller.New(m, fake, fake, controller.Options{})
	res := admit(t, m, store.TimeoutSpec{})
	ctx := context.Background()
	_ = c.Reconcile(ctx)
	name := k8s.PodName(res.Sandbox.ID)
	fake.SetScheduled(name, "gke-node-9")
	fake.SetReady(name, k8s.FakeGPUUUID)
	_ = c.Reconcile(ctx)

	if fake.GPUReusePending("gke-node-9") {
		t.Fatal("node tainted before teardown was requested")
	}
	if _, err := m.TerminateSandbox(ctx, "prj_dev", res.Sandbox.ID, "alice", "t", "h", "tr"); err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if !fake.GPUReusePending("gke-node-9") {
		t.Fatal("node was not tainted reuse-pending on GPU sandbox teardown")
	}
}

// TestCPUTeardownDoesNotTaintNode guards against over-applying F3's fix: CPU
// sandboxes have no GPU to protect and must not pay the reuse-pending gate.
func TestCPUTeardownDoesNotTaintNode(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	c := controller.New(m, fake, fake, controller.Options{})
	m.SeedImage("prj_dev", "img_seed")
	ctx := context.Background()
	res, err := m.CreateSandbox(ctx, store.CreateSandboxInput{
		ProjectID: "prj_dev", Principal: "alice", IdemKey: "cpu", IdemHash: "cpu",
		ImageID:   "img_seed",
		Resources: store.ResourceSpec{CPUMilli: 1000, MemoryMiB: 2048, Accelerator: store.AcceleratorSpec{Type: store.AcceleratorNone}},
		Timeouts:  store.TimeoutSpec{StartupSeconds: 120},
		MaxActive: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Reconcile(ctx)
	name := k8s.PodName(res.Sandbox.ID)
	fake.SetScheduled(name, "cpu-node-1")
	fake.SetKubeReady(name)
	_ = c.Reconcile(ctx)
	if _, err := m.TerminateSandbox(ctx, "prj_dev", res.Sandbox.ID, "alice", "t", "h", "tr"); err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if fake.GPUReusePending("cpu-node-1") {
		t.Fatal("CPU sandbox teardown tainted the node")
	}
}

func TestLeasePreventsStandbyMutations(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	leader := controller.New(m, fake, fake, controller.Options{HolderID: "a", Now: clock, LeaseTTL: 10 * time.Second})
	standby := controller.New(m, fake, fake, controller.Options{HolderID: "b", Now: clock, LeaseTTL: 10 * time.Second})
	admit(t, m, store.TimeoutSpec{})
	if err := leader.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.Creates != 1 {
		t.Fatal(fake.Creates)
	}
	if err := standby.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.Creates != 1 {
		t.Fatalf("standby mutated cluster: %d", fake.Creates)
	}
}

// leaseFlipStore wraps *store.Memory and makes every HoldLease call after the
// first fail, simulating another replica taking the lease mid-pass.
type leaseFlipStore struct {
	*store.Memory
	calls int
}

func (s *leaseFlipStore) HoldLease(ctx context.Context, holder string, now time.Time, ttl time.Duration) (bool, error) {
	s.calls++
	if s.calls > 1 {
		return false, nil
	}
	return s.Memory.HoldLease(ctx, holder, now, ttl)
}

// TestReconcileStopsMutatingOnMidPassLeaseLoss guards the fix in
// reconcileOnce: a long pass (bounded ListSandboxesAll can still return many
// active rows) must re-check lease ownership every leaseRenewBatch sandboxes,
// not just once at the start, so a standby that has since taken the lease
// cannot race the previous holder's Pod mutations for the rest of the pass.
func TestReconcileStopsMutatingOnMidPassLeaseLoss(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	wrapped := &leaseFlipStore{Memory: m}
	c := controller.New(wrapped, fake, fake, controller.Options{HolderID: "a", LeaseTTL: 10 * time.Second})

	m.SeedImage("prj_dev", "img_seed")
	const total = 55
	for i := 0; i < total; i++ {
		key := fmt.Sprintf("k%d", i)
		if _, err := m.CreateSandbox(context.Background(), store.CreateSandboxInput{
			ProjectID: "prj_dev", Principal: "alice", IdemKey: key, IdemHash: key,
			ImageID:   "img_seed",
			Resources: store.ResourceSpec{CPUMilli: 1000, MemoryMiB: 2048, Accelerator: store.AcceleratorSpec{Count: 1, Type: store.AcceleratorNVIDIAL4}},
			Timeouts:  store.TimeoutSpec{StartupSeconds: 120},
			MaxActive: total,
		}); err != nil {
			t.Fatal(err)
		}
	}

	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.Creates != 50 {
		t.Fatalf("Creates = %d, want exactly leaseRenewBatch (50) before the mid-pass lease check stopped the pass", fake.Creates)
	}
}

func TestProcessStaysCreatingUntilInitObserves(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	c := controller.New(m, fake, fake, controller.Options{})
	res := admit(t, m, store.TimeoutSpec{})
	ctx := context.Background()
	_ = c.Reconcile(ctx)
	name := k8s.PodName(res.Sandbox.ID)
	fake.SetReady(name, "GPU-1")
	_ = c.Reconcile(ctx)
	p, _, err := m.CreateProcess(ctx, store.CreateProcessInput{
		ProjectID: "prj_dev", SandboxID: res.Sandbox.ID, Principal: "alice",
		IdemKey: "p", IdemHash: "ph", Command: []string{"true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := m.GetProcess(ctx, "prj_dev", res.Sandbox.ID, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "CREATING" {
		t.Fatalf("process state = %s, want CREATING until init reports", got.State)
	}
	pod, err := fake.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	if pod.Annotations[k8s.AnnotProcDesired] == "" {
		t.Fatal("desired process annotation missing")
	}
}

func TestProcessAdvancesWhenInitObserves(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	c := controller.New(m, fake, fake, controller.Options{})
	res := admit(t, m, store.TimeoutSpec{})
	ctx := context.Background()
	_ = c.Reconcile(ctx)
	name := k8s.PodName(res.Sandbox.ID)
	fake.SetReady(name, "GPU-1")
	_ = c.Reconcile(ctx)
	p, _, err := m.CreateProcess(ctx, store.CreateProcessInput{
		ProjectID: "prj_dev", SandboxID: res.Sandbox.ID, Principal: "alice",
		IdemKey: "p", IdemHash: "ph", Command: []string{"true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	fake.SetProcessObserved(name, p.ID, "RUNNING", nil)
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := m.GetProcess(ctx, "prj_dev", res.Sandbox.ID, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "RUNNING" {
		t.Fatalf("process state = %s", got.State)
	}
}

func TestBalloonsMatchMinWarm(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	c := controller.New(m, fake, fake, controller.Options{MinWarm: 2, MaxWarm: 8})
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	list, _ := fake.List()
	n := 0
	for _, p := range list {
		if p.Labels[k8s.LabelWorkload] == k8s.WorkloadBalloon {
			n++
			if p.Spec.PriorityClassName != k8s.PriorityBalloon {
				t.Fatal("balloon priority")
			}
			if p.Spec.Containers[0].GPU != "1" {
				t.Fatal("balloon gpu")
			}
		}
	}
	if n != 2 {
		t.Fatalf("balloons = %d", n)
	}
}

func TestBalloonsTrackRecentCreateRate(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	res := admit(t, m, store.TimeoutSpec{})
	now := res.Sandbox.CreateTime.Add(time.Second)
	c := controller.New(m, fake, fake, controller.Options{
		MaxWarm:           8,
		WarmWindow:        time.Minute,
		NodeProvisionTime: time.Minute,
		Now:               func() time.Time { return now },
	})
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	list, _ := fake.List()
	balloons := 0
	for _, p := range list {
		if p.Labels[k8s.LabelWorkload] == k8s.WorkloadBalloon && p.Annotations[k8s.AnnotGPUType] == store.AcceleratorNVIDIAL4 {
			balloons++
		}
	}
	// One create/minute * one minute replenishment * 1.3 safety, rounded up.
	if balloons != 2 {
		t.Fatalf("GPU balloons = %d, want 2", balloons)
	}
}

// GPU and CPU warm buffers scale independently: each class's MinWarm is met
// without the two classes' balloons being confused for one another, and a
// CPU balloon requests no GPU and lands on the CPU sandbox pool.
func TestBalloonsGPUAndCPUIndependent(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	c := controller.New(m, fake, fake, controller.Options{
		MinWarm: 2, MaxWarm: 8,
		MinWarmCPU: 1, MaxWarmCPU: 8,
	})
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	list, _ := fake.List()
	gpuBalloons, cpuBalloons := 0, 0
	for _, p := range list {
		if p.Labels[k8s.LabelWorkload] != k8s.WorkloadBalloon {
			continue
		}
		switch p.Annotations[k8s.AnnotGPUType] {
		case store.AcceleratorNVIDIAL4:
			gpuBalloons++
			if p.Spec.Containers[0].GPU != "1" {
				t.Fatal("GPU balloon must request a GPU")
			}
			if p.Spec.NodeSelector[k8s.NodePoolLabel] != k8s.GPUNodePoolValue {
				t.Fatalf("GPU balloon node pool = %v", p.Spec.NodeSelector)
			}
		case store.AcceleratorNone:
			cpuBalloons++
			if p.Spec.Containers[0].GPU != "" {
				t.Fatalf("CPU balloon must not request a GPU, got %q", p.Spec.Containers[0].GPU)
			}
			if p.Spec.NodeSelector[k8s.NodePoolLabel] != k8s.CPUNodePoolValue {
				t.Fatalf("CPU balloon node pool = %v", p.Spec.NodeSelector)
			}
		default:
			t.Fatalf("balloon with unexpected class annotation %q", p.Annotations[k8s.AnnotGPUType])
		}
	}
	if gpuBalloons != 2 {
		t.Fatalf("GPU balloons = %d, want 2", gpuBalloons)
	}
	if cpuBalloons != 1 {
		t.Fatalf("CPU balloons = %d, want 1", cpuBalloons)
	}
}

// With CPU warm capacity disabled (the default), only the GPU buffer forms —
// closing this gap is opt-in, not automatic, so existing deployments' node
// spend does not change on upgrade.
func TestBalloonsCPUDisabledByDefault(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	c := controller.New(m, fake, fake, controller.Options{MinWarm: 1, MaxWarm: 8})
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	list, _ := fake.List()
	for _, p := range list {
		if p.Labels[k8s.LabelWorkload] == k8s.WorkloadBalloon && p.Annotations[k8s.AnnotGPUType] == store.AcceleratorNone {
			t.Fatal("CPU balloon created with CPU warm capacity disabled")
		}
	}
}

// The controller records a startup-stage latency sample the moment a
// sandbox first reaches each state, and only then — never on a repeated
// observation of an already-reached state (which reconcileSandbox's rank
// guard prevents from calling observeWrite at all).
func TestReconcileRecordsStageLatency(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	reg := prometheus.NewRegistry()
	metrics := adminz.NewReconcileMetrics(reg, adminz.NewRecorder(10))
	c := controller.New(m, fake, fake, controller.Options{Metrics: metrics})
	res := admit(t, m, store.TimeoutSpec{})
	ctx := context.Background()

	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	name := k8s.PodName(res.Sandbox.ID)

	// Walk each transition separately (as TestReconcileStateMachine does):
	// observe() reports only the pod's current snapshot, so setting all three
	// booleans at once would skip straight to READY and record no SCHEDULED
	// or STARTED sample at all.
	fake.SetScheduled(name, "gke-node-1")
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	fake.SetRunning(name)
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	fake.SetReady(name, "GPU-1")
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	// Reconcile again: the sandbox is already READY, so this must not add a
	// second READY sample.
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]uint64{}
	for _, f := range families {
		if f.GetName() != "ignition_sandbox_stage_latency_seconds" {
			continue
		}
		for _, metric := range f.GetMetric() {
			for _, lbl := range metric.GetLabel() {
				if lbl.GetName() == "state" {
					counts[lbl.GetValue()] = metric.GetHistogram().GetSampleCount()
				}
			}
		}
	}
	if counts["READY"] != 1 {
		t.Fatalf("READY samples = %d, want exactly 1 (no re-observation)", counts["READY"])
	}
	if counts["SCHEDULED"] != 1 || counts["STARTED"] != 1 {
		t.Fatalf("stage samples = %+v, want one SCHEDULED and one STARTED", counts)
	}
}

func TestCPUSupervisorBackedKubeReadyIsPublicReady(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	c := controller.New(m, fake, fake, controller.Options{})
	m.SeedImage("prj_dev", "img_seed")
	res, err := m.CreateSandbox(context.Background(), store.CreateSandboxInput{
		ProjectID: "prj_dev", Principal: "alice", IdemKey: t.Name(), IdemHash: t.Name(),
		ImageID:   "img_seed",
		Resources: store.ResourceSpec{CPUMilli: 1000, MemoryMiB: 2048, Accelerator: store.AcceleratorSpec{Type: store.AcceleratorNone}},
		Timeouts:  store.TimeoutSpec{StartupSeconds: 120},
		MaxActive: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_ = c.Reconcile(ctx)
	name := k8s.PodName(res.Sandbox.ID)
	fake.SetKubeReady(name)
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if mustGet(t, m, res.Sandbox.ID).State != "READY" {
		t.Fatalf("CPU sandbox: supervisor-backed PodReady must be READY, got %s", mustGet(t, m, res.Sandbox.ID).State)
	}
}

// A CPU sandbox with no sandbox-init in its image (NativeEntrypoint) must
// still reach public READY on kubelet PodReady alone — the only signal
// available when there is no /readyz to gate on — and its Pod must run the
// image's own entrypoint, not /ignition/init.
func TestCPUNativeEntrypointKubeReadyIsPublicReady(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	c := controller.New(m, fake, fake, controller.Options{})
	m.SeedImage("prj_dev", "img_seed")
	res, err := m.CreateSandbox(context.Background(), store.CreateSandboxInput{
		ProjectID: "prj_dev", Principal: "alice", IdemKey: t.Name(), IdemHash: t.Name(),
		ImageID:          "img_seed",
		NativeEntrypoint: true,
		Resources:        store.ResourceSpec{CPUMilli: 1000, MemoryMiB: 2048, Accelerator: store.AcceleratorSpec{Type: store.AcceleratorNone}},
		Timeouts:         store.TimeoutSpec{StartupSeconds: 120},
		MaxActive:        10,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_ = c.Reconcile(ctx)
	name := k8s.PodName(res.Sandbox.ID)
	pod, err := fake.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	if len(pod.Spec.Containers[0].Command) != 0 {
		t.Fatalf("native entrypoint sandbox must not run /ignition/init, got %v", pod.Spec.Containers[0].Command)
	}
	fake.SetKubeReady(name)
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if mustGet(t, m, res.Sandbox.ID).State != "READY" {
		t.Fatalf("native entrypoint CPU sandbox: PodReady must be READY, got %s", mustGet(t, m, res.Sandbox.ID).State)
	}
}

// A GPU sandbox must NOT reach public READY on kubelet PodReady alone: it also
// needs ignition-gpu-agent's attestation (canonical UUID + init-healthy), which
// the sandbox Pod cannot write for itself.
func TestGPUKubeReadyWithoutAgentAttestationStaysStarted(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	c := controller.New(m, fake, fake, controller.Options{})
	res := admit(t, m, store.TimeoutSpec{})
	ctx := context.Background()
	_ = c.Reconcile(ctx)
	name := k8s.PodName(res.Sandbox.ID)

	fake.SetKubeReady(name)
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, m, res.Sandbox.ID).State; got != "STARTED" {
		t.Fatalf("GPU sandbox without attestation: want STARTED, got %s", got)
	}

	// ignition-gpu-agent attests: canonical UUID + init-healthy.
	if err := fake.PatchAnnotations(name, map[string]string{
		k8s.AnnotGPUUUID:     k8s.FakeGPUUUID,
		k8s.AnnotInitHealthy: "true",
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, m, res.Sandbox.ID).State; got != "READY" {
		t.Fatalf("GPU sandbox after attestation: want READY, got %s", got)
	}
}

// A non-canonical UUID annotation (a device index, a device-node name) never
// satisfies the READY gate.
func TestGPUNonCanonicalUUIDAnnotationDoesNotGateReady(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	c := controller.New(m, fake, fake, controller.Options{})
	res := admit(t, m, store.TimeoutSpec{})
	ctx := context.Background()
	_ = c.Reconcile(ctx)
	name := k8s.PodName(res.Sandbox.ID)
	fake.SetKubeReady(name)
	if err := fake.PatchAnnotations(name, map[string]string{
		k8s.AnnotGPUUUID:     "nvidia0",
		k8s.AnnotInitHealthy: "true",
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, m, res.Sandbox.ID).State; got == "READY" {
		t.Fatalf("non-canonical UUID must not gate READY, got %s", got)
	}
}

func TestResolveImageUsesConfiguredPrefix(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	c := controller.New(m, fake, fake, controller.Options{
		ImagePrefix: "us-central1-docker.pkg.dev/my-gcp/sandboxes",
	})
	res := admit(t, m, store.TimeoutSpec{})
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	p, err := fake.Get(k8s.PodName(res.Sandbox.ID))
	if err != nil {
		t.Fatal(err)
	}
	want := "us-central1-docker.pkg.dev/my-gcp/sandboxes/img_seed"
	if p.Spec.Containers[0].Image != want {
		t.Fatalf("image = %q", p.Spec.Containers[0].Image)
	}
}

// A catalog image with a pinned RegistryRef (created via the image
// admission API, not SeedImage) schedules the Pod on that digest-pinned
// reference, never the mutable-tag prefix path.
func TestResolveImagePrefersCatalogDigest(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	c := controller.New(m, fake, fake, controller.Options{
		ImagePrefix: "us-central1-docker.pkg.dev/my-gcp/sandboxes",
	})
	if _, err := m.CreateImage(context.Background(), store.CreateImageInput{
		ProjectID: "prj_dev", ImageID: "img_pinned",
		SourceRef: "docker.io/library/nginx:1.27", Digest: "sha256:abc",
		RegistryRef: "docker.io/library/nginx@sha256:abc",
	}); err != nil {
		t.Fatal(err)
	}
	res, err := m.CreateSandbox(context.Background(), store.CreateSandboxInput{
		ProjectID: "prj_dev", Principal: "alice", IdemKey: t.Name(), IdemHash: t.Name(),
		ImageID:   "img_pinned",
		Resources: store.ResourceSpec{CPUMilli: 1000, MemoryMiB: 2048, Accelerator: store.AcceleratorSpec{Count: 1, Type: store.AcceleratorNVIDIAL4}},
		Timeouts:  store.TimeoutSpec{StartupSeconds: 120},
		MaxActive: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	p, err := fake.Get(k8s.PodName(res.Sandbox.ID))
	if err != nil {
		t.Fatal(err)
	}
	if p.Spec.Containers[0].Image != "docker.io/library/nginx@sha256:abc" {
		t.Fatalf("image = %q, want the pinned digest reference", p.Spec.Containers[0].Image)
	}
}

func TestInvalidImageIDDoesNotCreatePod(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	c := controller.New(m, fake, fake, controller.Options{})
	m.SeedImage("prj_dev", "evil/../other")
	res, err := m.CreateSandbox(context.Background(), store.CreateSandboxInput{
		ProjectID: "prj_dev",
		Principal: "alice",
		IdemKey:   t.Name(),
		IdemHash:  t.Name(),
		ImageID:   "evil/../other",
		Resources: store.ResourceSpec{CPUMilli: 1000, MemoryMiB: 2048, Accelerator: store.AcceleratorSpec{Count: 1, Type: store.AcceleratorNVIDIAL4}},
		Timeouts:  store.TimeoutSpec{StartupSeconds: 120},
		MaxActive: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.Creates != 0 {
		t.Fatalf("creates = %d", fake.Creates)
	}
	sb := mustGet(t, m, res.Sandbox.ID)
	if sb.State != "FAILED" || sb.StateReason != "IMAGE_UNAVAILABLE" {
		t.Fatalf("sandbox = %s %s", sb.State, sb.StateReason)
	}
}
