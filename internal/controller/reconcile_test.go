package controller_test

import (
	"context"
	"testing"
	"time"

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
		Resources: store.ResourceSpec{CPUMilli: 1000, MemoryMiB: 2048, GPU: store.GPUSpec{Count: 1, Type: store.GPUTypeNVIDIAL4}},
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

func TestKubeReadyIsNotPublicReady(t *testing.T) {
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
	if mustGet(t, m, res.Sandbox.ID).State != "STARTED" {
		t.Fatalf("kube Ready without init/GPU must be STARTED, got %s", mustGet(t, m, res.Sandbox.ID).State)
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
		Resources: store.ResourceSpec{CPUMilli: 1000, MemoryMiB: 2048, GPU: store.GPUSpec{Count: 1, Type: store.GPUTypeNVIDIAL4}},
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
