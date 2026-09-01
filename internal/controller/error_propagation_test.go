package controller_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"ignition.dev/ignition/internal/controller"
	"ignition.dev/ignition/internal/k8s"
	"ignition.dev/ignition/internal/store"
)

type getErrorPods struct {
	*k8s.Fake
	err error
}

func (p *getErrorPods) Get(string) (*k8s.Pod, error) { return nil, p.err }

func TestReconcileReturnsPerSandboxErrors(t *testing.T) {
	m := store.NewMemory()
	res := admit(t, m, store.TimeoutSpec{})
	want := errors.New("kubernetes unavailable")
	fake := k8s.NewFake()
	c := controller.New(m, &getErrorPods{Fake: fake, err: want}, fake, controller.Options{})

	err := c.Reconcile(context.Background())
	if !errors.Is(err, want) {
		t.Fatalf("Reconcile error = %v, want wrapped %v", err, want)
	}
	if !strings.Contains(err.Error(), res.Sandbox.ID) {
		t.Fatalf("Reconcile error %q does not identify the sandbox", err)
	}
	if c.Stats().ConsecutiveErrors != 1 {
		t.Fatalf("consecutive errors = %d, want 1", c.Stats().ConsecutiveErrors)
	}
}

type failProcessOnceStore struct {
	*store.Memory
	err    error
	failed bool
}

func (s *failProcessOnceStore) UpdateProcessObserved(ctx context.Context, in store.ProcessObserved) error {
	if !s.failed {
		s.failed = true
		return s.err
	}
	return s.Memory.UpdateProcessObserved(ctx, in)
}

func TestWorkerLossRetriesProcessesBeforeFailingSandbox(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	res := admit(t, m, store.TimeoutSpec{})
	ctx := context.Background()
	base := controller.New(m, fake, fake, controller.Options{})
	if err := base.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	fake.SetReady(k8s.PodName(res.Sandbox.ID), "GPU-1")
	if err := base.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	proc, _, err := m.CreateProcess(ctx, store.CreateProcessInput{
		ProjectID: "prj_dev", SandboxID: res.Sandbox.ID, Principal: "alice",
		IdemKey: t.Name(), IdemHash: t.Name(), Command: []string{"true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	fake.Drop(k8s.PodName(res.Sandbox.ID))

	want := errors.New("database unavailable")
	st := &failProcessOnceStore{Memory: m, err: want}
	c := controller.New(st, fake, fake, controller.Options{})
	if err := c.Reconcile(ctx); !errors.Is(err, want) {
		t.Fatalf("first Reconcile error = %v, want %v", err, want)
	}
	if got := mustGet(t, m, res.Sandbox.ID); got.State != "READY" {
		t.Fatalf("sandbox became terminal before its process: %s", got.State)
	}

	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, m, res.Sandbox.ID); got.State != "FAILED" {
		t.Fatalf("sandbox state = %s, want FAILED", got.State)
	}
	gotProc, err := m.GetProcess(ctx, "prj_dev", res.Sandbox.ID, proc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotProc.State != "FAILED" {
		t.Fatalf("process state = %s, want FAILED", gotProc.State)
	}
}
