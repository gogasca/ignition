package cli

import (
	"bytes"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ignition.dev/ignition/internal/api"
	"ignition.dev/ignition/internal/auth"
	"ignition.dev/ignition/internal/config"
	"ignition.dev/ignition/internal/store"
)

// TestCLIAgainstRealAPI runs ignitionctl against the actual api.Server (not the
// fake), so a drift between the CLI's request paths and the server's routes is
// caught here.
func TestCLIAgainstRealAPI(t *testing.T) {
	mem := store.NewMemory()
	mem.SeedRole("prj_dev", "alice", auth.RoleOwner)
	mem.SeedImage("prj_dev", "img_seed")
	srv := api.New(config.Config{
		EnabledRegion:      "us-central1",
		GatewayURL:         "https://gateway.us-central1.ignition.dev",
		StreamTokenSecret:  "test-stream-secret",
		MaxActiveSandboxes: 5,
		OIDCAudience:       "https://api.ignition.dev",
	}, mem, auth.Static{Tokens: map[string]auth.Principal{
		"t0ken": {Subject: "alice", Email: "alice@acme.test", Kind: auth.KindUser, Domain: "acme.test"},
	}})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	cfg := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("IGNITION_CONFIG", cfg)
	t.Setenv("IGNITION_SERVER", "")
	t.Setenv("IGNITION_TOKEN", "")
	t.Setenv("IGNITION_PROJECT", "")
	pollInterval = 5 * time.Millisecond

	run := func(args ...string) (string, string, error) {
		var out, errb bytes.Buffer
		err := run(nil, &out, &errb, args)
		return out.String(), errb.String(), err
	}

	if out, errb, err := run("login", "--server", ts.URL, "--token", "t0ken", "--project", "prj_dev"); err != nil {
		t.Fatalf("login: %v (stderr %s out %s)", err, errb, out)
	}
	if out, _, err := run("whoami"); err != nil || !strings.Contains(out, "alice") {
		t.Fatalf("whoami: out=%q err=%v", out, err)
	}

	out, errb, err := run("sandbox", "create", "--image", "img_seed", "--accelerator", "NONE", "--cpu", "1000", "--memory", "2048", "-o", "json")
	if err != nil {
		t.Fatalf("sandbox create: %v (stderr %s)", err, errb)
	}
	if !strings.Contains(out, `"state": "CREATING"`) && !strings.Contains(out, `"state":"CREATING"`) {
		t.Fatalf("create output = %s", out)
	}

	if out, errb, err := run("sandbox", "list"); err != nil {
		t.Fatalf("sandbox list: %v (stderr %s)", err, errb)
	} else if !strings.Contains(out, "sbx_") {
		t.Fatalf("list output = %s", out)
	}

	if out, errb, err := run("operation", "list"); err != nil {
		t.Fatalf("operation list: %v (stderr %s)", err, errb)
	} else if !strings.Contains(out, "CREATE_SANDBOX") {
		t.Fatalf("operation list output = %s", out)
	}
}
