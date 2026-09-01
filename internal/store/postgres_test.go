package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"ignition.dev/ignition/internal/auth"
	"ignition.dev/ignition/internal/store"
)

var postgresTestDSN string

func TestMain(m *testing.M) {
	if dsn := os.Getenv("IGNITION_TEST_DATABASE_URL"); dsn != "" {
		postgresTestDSN = dsn
		os.Exit(m.Run())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	container, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("ignition_test"),
		tcpostgres.WithUsername("ignition"),
		tcpostgres.WithPassword("ignition"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		cancel()
		fmt.Fprintf(os.Stderr, "start PostgreSQL test container: %v\n", err)
		os.Exit(1)
	}

	postgresTestDSN, err = container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = container.Terminate(ctx)
		cancel()
		fmt.Fprintf(os.Stderr, "get PostgreSQL test DSN: %v\n", err)
		os.Exit(1)
	}
	cancel()

	code := m.Run()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := container.Terminate(stopCtx); err != nil {
		fmt.Fprintf(os.Stderr, "stop PostgreSQL test container: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	stopCancel()
	os.Exit(code)
}

func postgresForTest(t *testing.T) *store.Postgres {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	p, err := store.OpenPostgres(ctx, postgresTestDSN)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func TestPostgresCreateSandboxIdempotency(t *testing.T) {
	ctx := context.Background()
	p := postgresForTest(t)
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
	p := postgresForTest(t)
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
	p := postgresForTest(t)
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
