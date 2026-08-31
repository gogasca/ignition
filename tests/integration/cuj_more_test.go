package integration_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"ignition.dev/ignition/internal/config"
	"ignition.dev/ignition/internal/id"
	"ignition.dev/ignition/internal/probe"
)

// TestCUJ_QuotaExhaustion — with a project cap of 1, a second create is rejected
// with QUOTA_EXCEEDED while the first sandbox holds the slot.
func TestCUJ_QuotaExhaustion(t *testing.T) {
	w := newWorldWithConfig(t, func(c *config.Config) { c.MaxActiveSandboxes = 1 })
	startAutopilot(t, w)
	c := newProbeClient(w)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sb, _, err := c.CreateSandbox(ctx, id.New("idem"), probe.CreateSandboxReq{
		ImageID: "img_seed",
		Command: []string{"sleep", "3600"},
	})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	t.Cleanup(func() { _, _ = c.TerminateSandbox(context.Background(), sb.ID, id.New("idem")) })
	if _, err := c.PollSandbox(ctx, sb.ID, func(s probe.SandboxView) (bool, error) {
		return s.State == "READY", nil
	}); err != nil {
		t.Fatalf("first sandbox never ready: %v", err)
	}

	_, _, err = c.CreateSandbox(ctx, id.New("idem"), probe.CreateSandboxReq{ImageID: "img_seed"})
	if !probe.CodeIs(err, "QUOTA_EXCEEDED") {
		t.Fatalf("second create: want QUOTA_EXCEEDED, got %v", err)
	}
}

// TestCUJ_RBACViewerCannotCreate — a viewer token is denied sandbox.create.
func TestCUJ_RBACViewerCannotCreate(t *testing.T) {
	w := newWorld(t)
	viewer := probe.New(w.ts.URL, "prj_dev", probe.WithStaticToken("viewer"))
	_, _, err := viewer.CreateSandbox(context.Background(), id.New("idem"), probe.CreateSandboxReq{ImageID: "img_seed"})
	if !probe.CodeIs(err, "PERMISSION_DENIED") {
		t.Fatalf("want PERMISSION_DENIED, got %v", err)
	}
}

// TestCUJ_CancelCreateFailsSandbox — cancelling the CREATE_SANDBOX operation
// fails the sandbox with reason CANCELLED (HTTP-driven, via the probe client).
func TestCUJ_CancelCreateFailsSandbox(t *testing.T) {
	w := newWorld(t)
	c := newProbeClient(w)
	ctx := context.Background()

	sb, op, err := c.CreateSandbox(ctx, id.New("idem"), probe.CreateSandboxReq{ImageID: "img_seed"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.CancelOperation(ctx, op.ID, id.New("idem"))
	if err != nil {
		t.Fatalf("cancel operation: %v", err)
	}
	if got.State != "CANCELLED" {
		t.Fatalf("operation state = %s", got.State)
	}
	after, err := c.GetSandbox(ctx, sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "FAILED" || after.StateReason != "CANCELLED" {
		t.Fatalf("sandbox = %s/%s, want FAILED/CANCELLED", after.State, after.StateReason)
	}
}

// TestCUJ_WatchSnapshotSSE — the sandbox :watch endpoint streams one snapshot
// event followed by heartbeats.
func TestCUJ_WatchSnapshotSSE(t *testing.T) {
	w := newWorld(t)
	sbx, _ := w.createSandbox(t, "watch-cuj")

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		w.ts.URL+"/v1/projects/prj_dev/sandboxes/"+sbx+":watch", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer alice")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "event: snapshot") {
		t.Fatalf("missing snapshot event in:\n%s", body)
	}
	if !strings.Contains(string(body), sbx) {
		t.Fatalf("snapshot data does not mention sandbox id")
	}
}
