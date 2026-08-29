package store_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"ignition.dev/ignition/internal/auth"
	"ignition.dev/ignition/internal/store"
)

func postgresOrSkip(t *testing.T) *store.Postgres {
	t.Helper()
	dsn := os.Getenv("IGNITION_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set IGNITION_TEST_DATABASE_URL to run Cloud SQL/Postgres store tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	p, err := store.OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func TestPostgresCreateSandboxIdempotency(t *testing.T) {
	ctx := context.Background()
	p := postgresOrSkip(t)
	project := "prj_pg_" + t.Name()
	p.SeedImage(project, "img")
	in := store.CreateSandboxInput{
		ProjectID: project,
		Principal: "alice",
		IdemKey:   "k1",
		IdemHash:  "hash-a",
		ImageID:   "img",
		Resources: spec(),
		MaxActive: 10,
	}
	a, err := p.CreateSandbox(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.CreateSandbox(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if b.Replay == nil || b.Replay.Status != 202 {
		t.Fatalf("expected replay, got %+v", b)
	}
	in.IdemHash = "hash-b"
	if _, err := p.CreateSandbox(ctx, in); !errors.Is(err, store.ErrIdempotencyReused) {
		t.Fatalf("err = %v", err)
	}
	got, err := p.GetSandbox(ctx, project, a.Sandbox.ID)
	if err != nil || got.ID != a.Sandbox.ID {
		t.Fatalf("get = %+v err=%v", got, err)
	}
}

func TestPostgresQuotaAndLease(t *testing.T) {
	ctx := context.Background()
	p := postgresOrSkip(t)
	project := "prj_pg_" + t.Name()
	p.SeedImage(project, "img")
	res, err := p.CreateSandbox(ctx, store.CreateSandboxInput{
		ProjectID: project, Principal: "alice", IdemKey: "c", IdemHash: "h",
		ImageID: "img", Resources: spec(), MaxActive: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.QuotaActive(project) != 1 {
		t.Fatalf("quota = %d", p.QuotaActive(project))
	}
	if _, err := p.CreateSandbox(ctx, store.CreateSandboxInput{
		ProjectID: project, Principal: "alice", IdemKey: "c2", IdemHash: "h2",
		ImageID: "img", Resources: spec(), MaxActive: 1,
	}); !errors.Is(err, store.ErrQuotaExceeded) {
		t.Fatalf("err = %v", err)
	}
	if err := p.UpdateObserved(ctx, store.ObservedUpdate{
		ProjectID: project, SandboxID: res.Sandbox.ID, State: "FAILED", Reason: "WORKER_LOST",
	}); err != nil {
		t.Fatal(err)
	}
	if p.QuotaActive(project) != 0 {
		t.Fatalf("quota after fail = %d", p.QuotaActive(project))
	}

	if err := p.ResetLeaseForTest(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ok, err := p.HoldLease(ctx, "a-"+t.Name(), now, 10*time.Second)
	if err != nil || !ok {
		t.Fatalf("a: %v %v", ok, err)
	}
	ok, err = p.HoldLease(ctx, "b-"+t.Name(), now.Add(time.Second), 10*time.Second)
	if err != nil || ok {
		t.Fatal("b should not steal unexpired lease")
	}
	ok, err = p.HoldLease(ctx, "b-"+t.Name(), now.Add(11*time.Second), 10*time.Second)
	if err != nil || !ok {
		t.Fatal("b should take expired lease")
	}
}

func TestPostgresRoleLookup(t *testing.T) {
	p := postgresOrSkip(t)
	project := "prj_pg_" + t.Name()
	p.SeedRole(project, "alice", auth.RoleOwner)
	role, ok, err := p.Role(context.Background(), project, "alice")
	if err != nil || !ok || role != auth.RoleOwner {
		t.Fatalf("role=%s ok=%v err=%v", role, ok, err)
	}
	if _, ok, err := p.Role(context.Background(), project, "bob"); err != nil || ok {
		t.Fatalf("missing binding ok=%v err=%v", ok, err)
	}
}
