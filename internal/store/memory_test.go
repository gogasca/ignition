package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"ignition.dev/ignition/internal/auth"
	"ignition.dev/ignition/internal/store"
)

func spec() store.ResourceSpec {
	return store.ResourceSpec{
		CPUMilli:    1000,
		MemoryMiB:   2048,
		Accelerator: store.AcceleratorSpec{Count: 1, Type: store.AcceleratorNVIDIAL4},
	}
}

func TestCreateSandboxIdempotency(t *testing.T) {
	ctx := context.Background()
	m := store.NewMemory()
	m.SeedImage("prj", "img")
	in := store.CreateSandboxInput{
		ProjectID: "prj",
		Principal: "alice",
		IdemKey:   "k1",
		IdemHash:  "hash-a",
		ImageID:   "img",
		Resources: spec(),
		MaxActive: 10,
	}
	a, err := m.CreateSandbox(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.CreateSandbox(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if b.Replay == nil || b.Replay.Status != 202 {
		t.Fatalf("expected replay, got %+v", b)
	}
	in.IdemHash = "hash-b"
	if _, err := m.CreateSandbox(ctx, in); !errors.Is(err, store.ErrIdempotencyReused) {
		t.Fatalf("err = %v", err)
	}
	_ = a
}

func TestCreateSandboxImageAndQuota(t *testing.T) {
	ctx := context.Background()
	m := store.NewMemory()
	in := store.CreateSandboxInput{
		ProjectID: "prj",
		Principal: "alice",
		IdemKey:   "k",
		IdemHash:  "h",
		ImageID:   "missing",
		Resources: spec(),
		MaxActive: 1,
	}
	if _, err := m.CreateSandbox(ctx, in); !errors.Is(err, store.ErrImageNotReady) {
		t.Fatalf("err = %v", err)
	}
	m.SeedImage("prj", "img")
	in.ImageID = "img"
	in.IdemKey = "k2"
	if _, err := m.CreateSandbox(ctx, in); err != nil {
		t.Fatal(err)
	}
	in.IdemKey = "k3"
	in.IdemHash = "h3"
	if _, err := m.CreateSandbox(ctx, in); !errors.Is(err, store.ErrQuotaExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestTerminateReleasesQuota(t *testing.T) {
	ctx := context.Background()
	m := store.NewMemory()
	m.SeedImage("prj", "img")
	res, err := m.CreateSandbox(ctx, store.CreateSandboxInput{
		ProjectID: "prj", Principal: "alice", IdemKey: "c", IdemHash: "h",
		ImageID: "img", Resources: spec(), MaxActive: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.TerminateSandbox(ctx, "prj", res.Sandbox.ID, "alice", "t", "th", "trace"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateSandbox(ctx, store.CreateSandboxInput{
		ProjectID: "prj", Principal: "alice", IdemKey: "c2", IdemHash: "h2",
		ImageID: "img", Resources: spec(), MaxActive: 1,
	}); err != nil {
		t.Fatalf("quota not released: %v", err)
	}
}

func TestProcessRequiresReady(t *testing.T) {
	ctx := context.Background()
	m := store.NewMemory()
	m.SeedImage("prj", "img")
	res, err := m.CreateSandbox(ctx, store.CreateSandboxInput{
		ProjectID: "prj", Principal: "alice", IdemKey: "c", IdemHash: "h",
		ImageID: "img", Resources: spec(), MaxActive: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = m.CreateProcess(ctx, store.CreateProcessInput{
		ProjectID: "prj", SandboxID: res.Sandbox.ID, Principal: "alice",
		IdemKey: "p", IdemHash: "ph", Command: []string{"true"},
	})
	if !errors.Is(err, store.ErrFailedPrecondition) {
		t.Fatalf("err = %v", err)
	}
	m.SetSandboxState("prj", res.Sandbox.ID, "READY")
	p, _, err := m.CreateProcess(ctx, store.CreateProcessInput{
		ProjectID: "prj", SandboxID: res.Sandbox.ID, Principal: "alice",
		IdemKey: "p2", IdemHash: "ph2", Command: []string{"true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.GetProcess(ctx, "prj", res.Sandbox.ID, p.ID)
	if err != nil || got.ID != p.ID {
		t.Fatalf("get = %+v err=%v", got, err)
	}
}

func TestRoleLookup(t *testing.T) {
	m := store.NewMemory()
	m.SeedRole("prj", "alice", auth.RoleOwner)
	role, ok, err := m.Role(context.Background(), "prj", "alice")
	if err != nil || !ok || role != auth.RoleOwner {
		t.Fatalf("role=%s ok=%v err=%v", role, ok, err)
	}
	if _, ok, err := m.Role(context.Background(), "prj", "bob"); err != nil || ok {
		t.Fatalf("missing binding ok=%v err=%v", ok, err)
	}
}

func TestListPagination(t *testing.T) {
	ctx := context.Background()
	m := store.NewMemory()
	m.SeedImage("prj", "img")
	for i := 0; i < 3; i++ {
		if _, err := m.CreateSandbox(ctx, store.CreateSandboxInput{
			ProjectID: "prj", Principal: "alice", IdemKey: string(rune('a' + i)), IdemHash: string(rune('A' + i)),
			ImageID: "img", Resources: spec(), MaxActive: 10,
		}); err != nil {
			t.Fatal(err)
		}
	}
	page, next, err := m.ListSandboxes(ctx, "prj", 2, "")
	if err != nil || len(page) != 2 || next == "" {
		t.Fatalf("page=%d next=%q err=%v", len(page), next, err)
	}
	rest, next2, err := m.ListSandboxes(ctx, "prj", 2, next)
	if err != nil || len(rest) != 1 || next2 != "" {
		t.Fatalf("rest=%d next=%q err=%v", len(rest), next2, err)
	}
}

func TestGetUnknownIsNotFound(t *testing.T) {
	m := store.NewMemory()
	if _, err := m.GetSandbox(context.Background(), "prj", "sbx_nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestHoldLease(t *testing.T) {
	m := store.NewMemory()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ok, err := m.HoldLease(context.Background(), "a", now, 10*time.Second)
	if err != nil || !ok {
		t.Fatalf("a: %v %v", ok, err)
	}
	ok, err = m.HoldLease(context.Background(), "b", now.Add(time.Second), 10*time.Second)
	if err != nil || ok {
		t.Fatal("b should not steal unexpired lease")
	}
	ok, err = m.HoldLease(context.Background(), "b", now.Add(11*time.Second), 10*time.Second)
	if err != nil || !ok {
		t.Fatal("b should take expired lease")
	}
}

func TestUpdateObservedReleasesQuotaOnFail(t *testing.T) {
	ctx := context.Background()
	m := store.NewMemory()
	m.SeedImage("prj", "img")
	res, err := m.CreateSandbox(ctx, store.CreateSandboxInput{
		ProjectID: "prj", Principal: "alice", IdemKey: "c", IdemHash: "h",
		ImageID: "img", Resources: spec(), MaxActive: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if m.QuotaActive("prj") != 1 {
		t.Fatal(m.QuotaActive("prj"))
	}
	if err := m.UpdateObserved(ctx, store.ObservedUpdate{
		ProjectID: "prj", SandboxID: res.Sandbox.ID, State: "FAILED", Reason: "WORKER_LOST",
	}); err != nil {
		t.Fatal(err)
	}
	if m.QuotaActive("prj") != 0 {
		t.Fatalf("quota = %d", m.QuotaActive("prj"))
	}
}
