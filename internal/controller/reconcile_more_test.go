package controller_test

import (
	"context"
	"testing"
	"time"

	"ignition.dev/ignition/internal/controller"
	"ignition.dev/ignition/internal/k8s"
	"ignition.dev/ignition/internal/secrets"
	"ignition.dev/ignition/internal/store"
)

func TestStartupTimeoutAfterScheduled(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	c := controller.New(m, fake, fake, controller.Options{Now: func() time.Time { return now }})
	sb := store.Sandbox{
		ID:        "sbx_timeoutsched0000001",
		ProjectID: "prj_dev",
		State:     "SCHEDULED",
		SandboxSpec: store.SandboxSpec{
			ImageID:   "img_seed",
			Timeouts:  store.TimeoutSpec{StartupSeconds: 30},
			Resources: store.ResourceSpec{CPUMilli: 1, MemoryMiB: 1},
		},
		CreateTime: now.Add(-2 * time.Minute),
	}
	m.SeedSandbox(sb)
	spec := k8s.SandboxPod(sb, "img")
	if err := fake.Create(spec); err != nil {
		t.Fatal(err)
	}
	fake.SetScheduled(k8s.PodName(sb.ID), "gke-node-1")
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := mustGet(t, m, sb.ID)
	if got.State != "FAILED" || got.StateReason != "STARTUP_TIMEOUT" {
		t.Fatalf("%s %s", got.State, got.StateReason)
	}
	if fake.Count() != 0 {
		t.Fatal("timed-out pod should be deleted")
	}
}

func TestReadyDoesNotRegress(t *testing.T) {
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
	fake.SetRunning(name)
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if mustGet(t, m, res.Sandbox.ID).State != "READY" {
		t.Fatalf("READY regressed to %s", mustGet(t, m, res.Sandbox.ID).State)
	}
}

func TestWorkerLostFailsProcesses(t *testing.T) {
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
	fake.Drop(name)
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := m.GetProcess(ctx, "prj_dev", res.Sandbox.ID, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "FAILED" {
		t.Fatalf("process state = %s", got.State)
	}
}

func TestBalloonsScaleDown(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	gpu, _ := k8s.ProfileFor(store.AcceleratorNVIDIAL4)
	for _, name := range []string{"balloon-nvidia-l4-0", "balloon-nvidia-l4-1", "balloon-nvidia-l4-2"} {
		if err := fake.Create(k8s.BalloonPod(name, gpu)); err != nil {
			t.Fatal(err)
		}
	}
	c := controller.New(m, fake, fake, controller.Options{MinWarm: 1, MaxWarm: 8})
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	list, _ := fake.List()
	n := 0
	for _, p := range list {
		if p.Labels[k8s.LabelWorkload] == k8s.WorkloadBalloon {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("balloons = %d, want 1", n)
	}
}

func TestBalloonsScaleDownWaitsForCooldown(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	gpu, _ := k8s.ProfileFor(store.AcceleratorNVIDIAL4)
	for _, name := range []string{"balloon-nvidia-l4-0", "balloon-nvidia-l4-1", "balloon-nvidia-l4-2"} {
		if err := fake.Create(k8s.BalloonPod(name, gpu)); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	c := controller.New(m, fake, fake, controller.Options{
		MinWarm: 1, MaxWarm: 8, BalloonCooldown: 15 * time.Minute,
		Now: func() time.Time { return now },
	})
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	n := balloonCount(t, fake)
	if n != 3 {
		t.Fatalf("during cooldown balloons = %d, want 3", n)
	}
	now = now.Add(15 * time.Minute)
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if balloonCount(t, fake) != 1 {
		t.Fatalf("after cooldown balloons = %d, want 1", balloonCount(t, fake))
	}
}

func balloonCount(t *testing.T, fake *k8s.Fake) int {
	t.Helper()
	list, _ := fake.List()
	n := 0
	for _, p := range list {
		if p.Labels[k8s.LabelWorkload] == k8s.WorkloadBalloon {
			n++
		}
	}
	return n
}

func TestLoopStopsOnCancel(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	c := controller.New(m, fake, fake, controller.Options{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Loop(ctx, 20*time.Millisecond) }()
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Loop did not return after cancel")
	}
}

func TestImagePullBackoff(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	c := controller.New(m, fake, fake, controller.Options{})
	res := admit(t, m, store.TimeoutSpec{})
	ctx := context.Background()
	_ = c.Reconcile(ctx)
	fake.SetFailed(k8s.PodName(res.Sandbox.ID), "ImagePullBackOff")
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	sb := mustGet(t, m, res.Sandbox.ID)
	if sb.StateReason != "IMAGE_UNAVAILABLE" {
		t.Fatalf("reason = %s", sb.StateReason)
	}
}

func TestSecretEnvInjectedAtPodCreate(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	c := controller.New(m, fake, fake, controller.Options{
		Secrets: secrets.Map{"sec_token": "s3cret"},
	})
	m.SeedImage("prj_dev", "img_seed")
	m.SeedSecret("prj_dev", "sec_token")
	res, err := m.CreateSandbox(context.Background(), store.CreateSandboxInput{
		ProjectID: "prj_dev",
		Principal: "alice",
		IdemKey:   t.Name(),
		IdemHash:  t.Name(),
		SandboxSpec: store.SandboxSpec{
			ImageID:   "img_seed",
			Resources: store.ResourceSpec{CPUMilli: 1000, MemoryMiB: 2048, Accelerator: store.AcceleratorSpec{Count: 1, Type: store.AcceleratorNVIDIAL4}},
			Timeouts:  store.TimeoutSpec{StartupSeconds: 120},
		},
		SecretRefs: []store.SecretRef{{SecretID: "sec_token", EnvironmentName: "MODEL_TOKEN"}},
		MaxActive:  10,
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
	env := p.Spec.Containers[0].Env
	if env["MODEL_TOKEN"] != "s3cret" {
		t.Fatalf("secret env = %v", env)
	}
}

func TestMissingSecretFailsWithoutPod(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	c := controller.New(m, fake, fake, controller.Options{Secrets: secrets.Map{}})
	m.SeedImage("prj_dev", "img_seed")
	// Registered to the project (admission passes) but absent from the
	// resolver, exercising the controller-level SECRET_UNAVAILABLE path
	// distinct from the project-registration check in store.CreateSandbox.
	m.SeedSecret("prj_dev", "missing")
	res, err := m.CreateSandbox(context.Background(), store.CreateSandboxInput{
		ProjectID: "prj_dev",
		Principal: "alice",
		IdemKey:   t.Name(),
		IdemHash:  t.Name(),
		SandboxSpec: store.SandboxSpec{
			ImageID:   "img_seed",
			Resources: store.ResourceSpec{CPUMilli: 1000, MemoryMiB: 2048, Accelerator: store.AcceleratorSpec{Count: 1, Type: store.AcceleratorNVIDIAL4}},
			Timeouts:  store.TimeoutSpec{StartupSeconds: 120},
		},
		SecretRefs: []store.SecretRef{{SecretID: "missing", EnvironmentName: "TOKEN"}},
		MaxActive:  10,
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
	if sb.StateReason != "SECRET_UNAVAILABLE" {
		t.Fatalf("reason = %s", sb.StateReason)
	}
}

func TestScaleDownDisabledOnOccupiedNode(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	c := controller.New(m, fake, fake, controller.Options{})
	res := admit(t, m, store.TimeoutSpec{})
	ctx := context.Background()
	_ = c.Reconcile(ctx)
	name := k8s.PodName(res.Sandbox.ID)
	fake.SetScheduled(name, "gke-node-1")
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if !fake.ScaleDown["gke-node-1"] {
		t.Fatalf("scale-down-disabled = %v", fake.ScaleDown)
	}
	fake.Drop(name)
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if fake.ScaleDown["gke-node-1"] {
		t.Fatal("scale-down-disabled should be cleared after sandbox is gone")
	}
}

// admitWithMain creates a CREATING sandbox that already carries its supervised
// main process row (as a managed nativeEntrypoint=false create does).
func admitWithMain(t *testing.T, m *store.Memory) store.CreateSandboxResult {
	t.Helper()
	m.SeedImage("prj_dev", "img_seed")
	res, err := m.CreateSandbox(context.Background(), store.CreateSandboxInput{
		ProjectID: "prj_dev",
		Principal: "alice",
		IdemKey:   t.Name(),
		IdemHash:  t.Name(),
		SandboxSpec: store.SandboxSpec{
			ImageID:   "img_seed",
			Command:   []string{"sleep", "1"},
			Resources: store.ResourceSpec{CPUMilli: 1000, MemoryMiB: 2048, Accelerator: store.AcceleratorSpec{Count: 1, Type: store.AcceleratorNVIDIAL4}},
			Timeouts:  store.TimeoutSpec{StartupSeconds: 30},
		},
		MainCommand: []string{"sleep", "1"},
		MaxActive:   10,
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func onlyProcess(t *testing.T, m *store.Memory, sandboxID string) store.Process {
	t.Helper()
	procs, err := m.ListProcessesBySandbox(context.Background(), "prj_dev", sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if len(procs) != 1 {
		t.Fatalf("want exactly 1 process, got %d", len(procs))
	}
	return procs[0]
}

// B1: a CREATING sandbox whose Pod was never created must fail its main
// process row in the same pass it transitions to FAILED, not one tick later.
func TestCreatingMissingPodFailsMainProcess(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	c := controller.New(m, fake, fake, controller.Options{Now: func() time.Time { return now }})
	res := admitWithMain(t, m)
	// Push past the startup deadline with no Pod ever created.
	sb := mustGet(t, m, res.Sandbox.ID)
	sb.CreateTime = now.Add(-2 * time.Minute)
	m.SeedSandbox(sb)

	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, m, res.Sandbox.ID); got.State != "FAILED" || got.StateReason != "CAPACITY_UNAVAILABLE" {
		t.Fatalf("sandbox = %s/%s", got.State, got.StateReason)
	}
	if p := onlyProcess(t, m, res.Sandbox.ID); p.State != "FAILED" {
		t.Fatalf("main process state = %s, want FAILED in the same pass", p.State)
	}
}

// B3: terminating a READY sandbox must fail its child processes in the same
// pass that tears the Pod down, not leave them RUNNING for two ticks.
func TestTerminatingFailsProcessesImmediately(t *testing.T) {
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
	_ = c.Reconcile(ctx)

	if _, err := m.TerminateSandbox(ctx, "prj_dev", res.Sandbox.ID, "alice", "t", "th", "trace"); err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := m.GetProcess(ctx, "prj_dev", res.Sandbox.ID, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "FAILED" {
		t.Fatalf("process state = %s, want FAILED on the terminating pass", got.State)
	}
}

// B4: a READY sandbox whose operation has already reached SUCCEEDED must
// surface a subsequent crash on that operation — the operation regresses
// SUCCEEDED→FAILED so a client watching :watch sees the runtime failure.
// This is deliberate: idle/runtime-limit teardown keeps the operation
// SUCCEEDED (creation succeeded), but a pod crash (WORKER_LOST) flips it to
// FAILED.
func TestWorkerLostSurfacesOnGET(t *testing.T) {
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
		t.Fatal("sandbox did not reach READY")
	}
	op, err := m.GetOperation(ctx, "prj_dev", res.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if op.State != "SUCCEEDED" {
		t.Fatalf("operation = %s, want SUCCEEDED after READY", op.State)
	}
	fake.Drop(name)
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	sb := mustGet(t, m, res.Sandbox.ID)
	if sb.State != "FAILED" || sb.StateReason != "WORKER_LOST" {
		t.Fatalf("sandbox = %s/%s", sb.State, sb.StateReason)
	}
	op, err = m.GetOperation(ctx, "prj_dev", res.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if op.State != "FAILED" {
		t.Fatalf("operation = %s, want FAILED (crash surfaces on operation)", op.State)
	}
	if op.EndTime == nil {
		t.Fatal("operation EndTime should be set on FAILED transition")
	}
}
