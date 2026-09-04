package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"ignition.dev/ignition/internal/store"
)

func TestCancelOperationFailsCreateSandbox(t *testing.T) {
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
	op, _, err := m.CancelOperation(ctx, "prj", res.Operation.ID, "alice", "k", "h")
	if err != nil {
		t.Fatal(err)
	}
	if op.State != "CANCELLED" {
		t.Fatalf("op = %s", op.State)
	}
	sb, err := m.GetSandbox(ctx, "prj", res.Sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sb.State != "FAILED" || sb.StateReason != "CANCELLED" {
		t.Fatalf("sandbox = %s %s", sb.State, sb.StateReason)
	}
	if m.QuotaActive("prj") != 0 {
		t.Fatalf("quota = %d", m.QuotaActive("prj"))
	}
}

func TestCancelOperationIdempotent(t *testing.T) {
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
	a, replay, err := m.CancelOperation(ctx, "prj", res.Operation.ID, "alice", "same", "hash")
	if err != nil || replay != nil {
		t.Fatalf("first: %+v %v", replay, err)
	}
	_, replay, err = m.CancelOperation(ctx, "prj", res.Operation.ID, "alice", "same", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if replay == nil {
		t.Fatal("expected replay")
	}
	_ = a
}

func TestListProcessesMissingSandboxIsEmpty(t *testing.T) {
	items, next, err := store.NewMemory().ListProcesses(context.Background(), "prj", "sbx_missing", 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if next != "" || len(items) != 0 {
		t.Fatalf("items=%d next=%q", len(items), next)
	}
}

func TestCreateProcessIdempotent(t *testing.T) {
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
	m.SetSandboxState("prj", res.Sandbox.ID, "READY")
	in := store.CreateProcessInput{
		ProjectID: "prj", SandboxID: res.Sandbox.ID, Principal: "alice",
		IdemKey: "p", IdemHash: "ph", Command: []string{"true"},
	}
	a, _, err := m.CreateProcess(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	_, replay, err := m.CreateProcess(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if replay == nil {
		t.Fatal("expected replay")
	}
	in.IdemHash = "other"
	if _, _, err := m.CreateProcess(ctx, in); !errors.Is(err, store.ErrIdempotencyReused) {
		t.Fatalf("err = %v", err)
	}
	_ = a
}

func TestUpdateObservedIgnoresTerminal(t *testing.T) {
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
	if err := m.UpdateObserved(ctx, store.ObservedUpdate{
		ProjectID: "prj", SandboxID: res.Sandbox.ID, State: "FAILED", Reason: "WORKER_LOST",
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.UpdateObserved(ctx, store.ObservedUpdate{
		ProjectID: "prj", SandboxID: res.Sandbox.ID, State: "READY", Reason: "READY",
	}); err != nil {
		t.Fatal(err)
	}
	sb, err := m.GetSandbox(ctx, "prj", res.Sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sb.State != "FAILED" {
		t.Fatalf("terminal sandbox mutated to %s", sb.State)
	}
}

// TestListSandboxesAllBoundsTerminalHistory guards the reconcile-cost fix: a
// terminal sandbox must drop out of ListSandboxesAll once it is older than
// ReconcileWindow, or a controller reconcile pass grows with total lifetime
// sandbox count instead of active-sandbox count.
func TestListSandboxesAllBoundsTerminalHistory(t *testing.T) {
	ctx := context.Background()
	m := store.NewMemory()
	now := time.Now().UTC()
	old := now.Add(-store.ReconcileWindow - time.Minute)
	recent := now.Add(-time.Minute)
	m.SeedSandbox(store.Sandbox{ID: "sbx_old_failed", ProjectID: "prj", State: "FAILED", FinishTime: &old, CreateTime: old})
	m.SeedSandbox(store.Sandbox{ID: "sbx_recent_finished", ProjectID: "prj", State: "FINISHED", FinishTime: &recent, CreateTime: recent})
	m.SeedSandbox(store.Sandbox{ID: "sbx_active", ProjectID: "prj", State: "READY", CreateTime: old})

	out, err := m.ListSandboxesAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, sb := range out {
		got[sb.ID] = true
	}
	if got["sbx_old_failed"] {
		t.Fatalf("old terminal sandbox was not excluded: %v", got)
	}
	if !got["sbx_recent_finished"] {
		t.Fatalf("recently terminal sandbox was excluded: %v", got)
	}
	if !got["sbx_active"] {
		t.Fatalf("active sandbox (old create_time, non-terminal) was excluded: %v", got)
	}
}

func TestGetSandboxWrongProjectIsNotFound(t *testing.T) {
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
	if _, err := m.GetSandbox(ctx, "other", res.Sandbox.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}
