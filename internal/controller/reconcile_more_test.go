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
		ID:         "sbx_timeoutsched0000001",
		ProjectID:  "prj_dev",
		State:      "SCHEDULED",
		ImageID:    "img_seed",
		CreateTime: now.Add(-2 * time.Minute),
		Timeouts:   store.TimeoutSpec{StartupSeconds: 30},
		Resources:  store.ResourceSpec{CPUMilli: 1, MemoryMiB: 1},
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
	for _, name := range []string{"balloon-0", "balloon-1", "balloon-2"} {
		if err := fake.Create(k8s.BalloonPod(name)); err != nil {
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
	for _, name := range []string{"balloon-0", "balloon-1", "balloon-2"} {
		if err := fake.Create(k8s.BalloonPod(name)); err != nil {
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
	res, err := m.CreateSandbox(context.Background(), store.CreateSandboxInput{
		ProjectID: "prj_dev", Principal: "alice", IdemKey: t.Name(), IdemHash: t.Name(),
		ImageID:    "img_seed",
		Resources:  store.ResourceSpec{CPUMilli: 1000, MemoryMiB: 2048, Accelerator: store.AcceleratorSpec{Count: 1, Type: store.AcceleratorNVIDIAL4}},
		Timeouts:   store.TimeoutSpec{StartupSeconds: 120},
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
	res, err := m.CreateSandbox(context.Background(), store.CreateSandboxInput{
		ProjectID: "prj_dev", Principal: "alice", IdemKey: t.Name(), IdemHash: t.Name(),
		ImageID:    "img_seed",
		Resources:  store.ResourceSpec{CPUMilli: 1000, MemoryMiB: 2048, Accelerator: store.AcceleratorSpec{Count: 1, Type: store.AcceleratorNVIDIAL4}},
		Timeouts:   store.TimeoutSpec{StartupSeconds: 120},
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
