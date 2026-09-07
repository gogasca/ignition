package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ignition.dev/ignition/internal/gateway"
	"ignition.dev/ignition/internal/sandboxinit"
	"ignition.dev/ignition/internal/streamtoken"
)

// fakeAPI is a minimal stand-in for ignition-api: enough surface for the CLI
// tests, with a per-sandbox state machine that advances on each GET.
type fakeAPI struct {
	mu         sync.Mutex
	t          *testing.T
	sandboxes  map[string]map[string]any
	procs      map[string]map[string]any
	getHits    map[string]int
	lastIdem   string
	gatewayURL string                        // when set, :attach hands it back so exec streams
	mintToken  func(processID string) string // when set, :attach mints a real stream token
}

func newFakeAPI(t *testing.T) (*httptest.Server, *fakeAPI) {
	f := &fakeAPI{
		t:         t,
		sandboxes: map[string]map[string]any{},
		procs:     map[string]map[string]any{},
		getHits:   map[string]int{},
	}
	return httptest.NewServer(f), f
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("X-Request-Id", "req_test")
	if k := r.Header.Get("Idempotency-Key"); k != "" {
		f.lastIdem = k
	}
	if r.Header.Get("Authorization") != "Bearer t0ken" {
		writeErr(w, 401, "UNAUTHENTICATED", "auth required")
		return
	}
	p := r.URL.Path
	switch {
	case p == "/v1/me":
		writeJSONT(w, 200, map[string]any{"subject": "alice", "domain": "acme.test"})
	case p == "/v1/me/projects":
		writeJSONT(w, 200, map[string]any{"projects": []map[string]any{{"id": "prj_dev", "name": "Dev", "domain": "acme.test"}}})
	case p == "/v1/sandboxes" && r.Method == "POST":
		id := "sbx_1"
		sb := map[string]any{"id": id, "projectId": "prj_dev", "state": "CREATING", "imageId": "img_seed",
			"resources": map[string]any{"cpuMilli": 1000, "memoryMiB": 2048, "accelerator": map[string]any{"type": "NVIDIA_L4", "count": 1}}}
		f.sandboxes[id] = sb
		writeJSONT(w, 202, map[string]any{"sandbox": sb, "operation": map[string]any{"id": "op_1", "state": "RUNNING", "kind": "CREATE_SANDBOX"}})
	case p == "/v1/sandboxes" && r.Method == "GET":
		var list []any
		for _, s := range f.sandboxes {
			list = append(list, s)
		}
		writeJSONT(w, 200, map[string]any{"sandboxes": list, "nextPageToken": ""})
	case strings.HasPrefix(p, "/v1/sandboxes/") && strings.HasSuffix(p, ":terminate"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/v1/sandboxes/"), ":terminate")
		sb := f.sandboxes[id]
		if sb == nil {
			writeErr(w, 404, "NOT_FOUND", "not found")
			return
		}
		sb["state"] = "FINISHED"
		writeJSONT(w, 202, map[string]any{"sandbox": sb, "operation": map[string]any{"id": "op_2", "state": "SUCCEEDED"}})
	case strings.HasPrefix(p, "/v1/sandboxes/") && strings.Contains(p, "/processes"):
		f.handleProcess(w, r, p)
	case strings.HasPrefix(p, "/v1/sandboxes/"):
		id := strings.TrimPrefix(p, "/v1/sandboxes/")
		sb := f.sandboxes[id]
		if sb == nil {
			writeErr(w, 404, "NOT_FOUND", "not found")
			return
		}
		f.getHits["sb:"+id]++
		if f.getHits["sb:"+id] >= 2 {
			sb["state"] = "READY"
		}
		writeJSONT(w, 200, sb)
	case strings.HasPrefix(p, "/v1/operations/"):
		id := strings.TrimPrefix(p, "/v1/operations/")
		if strings.HasSuffix(p, ":cancel") {
			writeJSONT(w, 200, map[string]any{"id": strings.TrimSuffix(id, ":cancel"), "state": "CANCELLED"})
			return
		}
		f.getHits["op:"+id]++
		state := "RUNNING"
		if f.getHits["op:"+id] >= 2 {
			state = "SUCCEEDED"
		}
		writeJSONT(w, 200, map[string]any{"id": id, "state": state, "kind": "CREATE_SANDBOX", "resourceId": "sbx_1"})
	case p == "/v1/operations":
		writeJSONT(w, 200, map[string]any{"operations": []map[string]any{{"id": "op_1", "state": "SUCCEEDED", "kind": "CREATE_SANDBOX", "resourceId": "sbx_1"}}, "nextPageToken": ""})
	default:
		writeErr(w, 404, "NOT_FOUND", "no route: "+p)
	}
}

func (f *fakeAPI) handleProcess(w http.ResponseWriter, r *http.Request, p string) {
	rest := strings.TrimPrefix(p, "/v1/sandboxes/")
	sbID, tail, _ := strings.Cut(rest, "/processes")
	tail = strings.TrimPrefix(tail, "/")
	switch {
	case tail == "" && r.Method == "POST":
		id := "prc_1"
		pr := map[string]any{"id": id, "sandboxId": sbID, "state": "CREATING", "command": []string{"echo", "hi"}}
		f.procs[id] = pr
		writeJSONT(w, 200, pr)
	case tail == "" && r.Method == "GET":
		var list []any
		for _, pr := range f.procs {
			list = append(list, pr)
		}
		writeJSONT(w, 200, map[string]any{"processes": list, "nextPageToken": ""})
	case strings.HasSuffix(tail, ":signal"):
		id := strings.TrimSuffix(tail, ":signal")
		f.procs[id]["state"] = "RUNNING"
		writeJSONT(w, 200, f.procs[id])
	case strings.HasSuffix(tail, ":cancel"):
		id := strings.TrimSuffix(tail, ":cancel")
		f.procs[id]["state"] = "CANCELLING"
		writeJSONT(w, 200, f.procs[id])
	case strings.HasSuffix(tail, ":attach"):
		// gatewayUrl empty => ignitionctl exec uses the polling path.
		tok := "st_x"
		if f.mintToken != nil {
			tok = f.mintToken(strings.TrimSuffix(tail, ":attach"))
		}
		writeJSONT(w, 200, map[string]any{"streamToken": tok, "gatewayUrl": f.gatewayURL, "streamEpoch": 7})
	default: // GET one process
		pr := f.procs[tail]
		if pr == nil {
			writeErr(w, 404, "NOT_FOUND", "no process")
			return
		}
		f.getHits["pr:"+tail]++
		if f.getHits["pr:"+tail] >= 2 {
			pr["state"] = "EXITED"
			pr["exitCode"] = _exitCode
		}
		writeJSONT(w, 200, pr)
	}
}

// _exitCode is the exit code the fake reports once a process reaches EXITED.
// Tests set it before invoking exec.
var _exitCode = 0

func writeJSONT(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, ecode, msg string) {
	writeJSONT(w, code, map[string]any{"code": ecode, "message": msg, "requestId": "req_test"})
}

// --- helpers ---

type harness struct {
	t      *testing.T
	server string
	cfg    string
	stdin  io.Reader
	api    *fakeAPI
}

func setup(t *testing.T) *harness {
	t.Helper()
	srv, api := newFakeAPI(t)
	t.Cleanup(srv.Close)
	cfg := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("IGNITION_CONFIG", cfg)
	pollInterval = 5 * time.Millisecond
	t.Setenv("IGNITION_SERVER", "")
	t.Setenv("IGNITION_TOKEN", "")
	t.Setenv("IGNITION_PROJECT", "")
	return &harness{t: t, server: srv.URL, cfg: cfg, api: api}
}

func (h *harness) run(args ...string) (string, string, error) {
	h.t.Helper()
	var out, errb bytes.Buffer
	err := run(h.stdin, &out, &errb, args)
	return out.String(), errb.String(), err
}

func (h *harness) login() {
	h.t.Helper()
	if _, _, err := h.run("login", "--server", h.server, "--token", "t0ken", "--project", "prj_dev"); err != nil {
		h.t.Fatalf("login: %v", err)
	}
}

// --- tests ---

func TestLoginPersistsAndVerifies(t *testing.T) {
	h := setup(t)
	out, _, err := h.run("login", "--server", h.server, "--token", "t0ken", "--project", "prj_dev")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if !strings.Contains(out, "Logged in") || !strings.Contains(out, "alice") {
		t.Fatalf("unexpected output: %q", out)
	}
	c, _ := loadConfig()
	if c.Server != h.server || c.Token != "t0ken" || c.Project != "prj_dev" {
		t.Fatalf("config not persisted: %+v", c)
	}
}

func TestLoginRejectsBadToken(t *testing.T) {
	h := setup(t)
	_, _, err := h.run("login", "--server", h.server, "--token", "wrong")
	if err == nil {
		t.Fatal("expected error for bad token")
	}
}

func TestWhoamiRequiresServer(t *testing.T) {
	h := setup(t)
	_, _, err := h.run("whoami")
	if ExitCode(err) != exitUsage {
		t.Fatalf("want usage exit, got %d (%v)", ExitCode(err), err)
	}
}

func TestSandboxCreateWait(t *testing.T) {
	h := setup(t)
	h.login()
	out, _, err := h.run("sandbox", "create", "--image", "img_seed", "--wait")
	if err != nil {
		t.Fatalf("create --wait: %v", err)
	}
	if !strings.Contains(out, "sbx_1") || !strings.Contains(out, "READY") {
		t.Fatalf("expected READY sandbox, got: %q", out)
	}
}

func TestSandboxCreateJSON(t *testing.T) {
	h := setup(t)
	h.login()
	out, _, err := h.run("-o", "json", "sandbox", "create", "--image", "img_seed")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if got["sandbox"] == nil || got["operation"] == nil {
		t.Fatalf("missing keys: %v", got)
	}
}

func TestSandboxListGetTerminate(t *testing.T) {
	h := setup(t)
	h.login()
	if _, _, err := h.run("sandbox", "create", "--image", "img_seed"); err != nil {
		t.Fatalf("seed create: %v", err)
	}
	if out, _, err := h.run("sandbox", "list"); err != nil || !strings.Contains(out, "sbx_1") {
		t.Fatalf("list: %v / %q", err, out)
	}
	if out, _, err := h.run("sandbox", "get", "sbx_1"); err != nil || !strings.Contains(out, "Image:") {
		t.Fatalf("get: %v / %q", err, out)
	}
	if out, _, err := h.run("sandbox", "terminate", "sbx_1"); err != nil || !strings.Contains(out, "FINISHED") {
		t.Fatalf("terminate: %v / %q", err, out)
	}
}

func TestExecPropagatesExitCode(t *testing.T) {
	h := setup(t)
	h.login()
	_exitCode = 0
	if _, _, err := h.run("exec", "sbx_1", "--", "echo", "hi"); err != nil {
		t.Fatalf("exec exit 0: %v", err)
	}
	_exitCode = 7
	// reset process hit counter by using a fresh harness server
	h2 := setup(t)
	h2.login()
	_, _, err := h2.run("exec", "sbx_1", "--", "false")
	if ExitCode(err) != 7 {
		t.Fatalf("want exit 7, got %d (%v)", ExitCode(err), err)
	}
	_exitCode = 0
}

func TestProcessSubcommands(t *testing.T) {
	h := setup(t)
	h.login()
	if _, _, err := h.run("exec", "--no-wait", "sbx_1", "--", "sleep", "1"); err != nil {
		t.Fatalf("exec --no-wait: %v", err)
	}
	if out, _, err := h.run("process", "list", "sbx_1"); err != nil || !strings.Contains(out, "prc_1") {
		t.Fatalf("process list: %v / %q", err, out)
	}
	if out, _, err := h.run("process", "signal", "sbx_1", "prc_1", "--signal", "SIGINT"); err != nil || !strings.Contains(out, "RUNNING") {
		t.Fatalf("signal: %v / %q", err, out)
	}
	if out, _, err := h.run("process", "cancel", "sbx_1", "prc_1"); err != nil || !strings.Contains(out, "CANCELLING") {
		t.Fatalf("cancel: %v / %q", err, out)
	}
}

func TestExecAttachToken(t *testing.T) {
	h := setup(t)
	h.login()
	out, _, err := h.run("exec", "--attach-token", "sbx_1", "--", "bash")
	if err != nil {
		t.Fatalf("attach-token: %v", err)
	}
	if !strings.Contains(out, "st_x") || !strings.Contains(out, "process:     prc_1") {
		t.Fatalf("missing attach fields: %q", out)
	}
}

func TestOperationWatch(t *testing.T) {
	h := setup(t)
	h.login()
	out, _, err := h.run("operation", "watch", "op_1")
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if !strings.Contains(out, "SUCCEEDED") {
		t.Fatalf("expected SUCCEEDED, got: %q", out)
	}
}

func TestNotFoundExitCode(t *testing.T) {
	h := setup(t)
	h.login()
	_, _, err := h.run("sandbox", "get", "sbx_missing")
	if ExitCode(err) != exitNotFound {
		t.Fatalf("want exit %d, got %d (%v)", exitNotFound, ExitCode(err), err)
	}
}

func TestUnknownCommand(t *testing.T) {
	h := setup(t)
	_, _, err := h.run("frobnicate")
	if ExitCode(err) != exitUsage {
		t.Fatalf("want usage exit, got %d", ExitCode(err))
	}
}

type stubResolver struct{ ep gateway.Endpoint }

func (s stubResolver) Resolve(context.Context, string, string) (gateway.Endpoint, error) {
	return s.ep, nil
}

func TestExecStreamsThroughGateway(t *testing.T) {
	const secret, aud = "cli-stream-secret", "https://gw.local"

	// Real sandbox-init supervisor running `cat`.
	dir := t.TempDir()
	df := filepath.Join(dir, "desired")
	if err := os.WriteFile(df, []byte(`{"prc_1":{"command":["cat"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	pm := sandboxinit.NewProcessManager(df, filepath.Join(dir, "work"))
	stop := make(chan struct{})
	defer close(stop)
	go pm.Run(stop, 20*time.Millisecond)
	supSrv := httptest.NewServer(sandboxinit.New(func() (string, error) { return "GPU-x", nil }).WithProcessManager(pm).Handler())
	defer supSrv.Close()

	// Real gateway pointed at the supervisor.
	gw := gateway.New(secret, aud, stubResolver{ep: gateway.Endpoint{
		PodIP: strings.TrimPrefix(supSrv.URL, "http://"), Ready: true,
	}})
	gwSrv := httptest.NewServer(gw.Handler())
	defer gwSrv.Close()

	h := setup(t)
	h.api.gatewayURL = gwSrv.URL
	h.api.mintToken = func(processID string) string {
		now := time.Now()
		tok, _ := streamtoken.Sign(secret, streamtoken.Claims{
			Subject: "dev", Audience: aud, ProjectID: "prj_dev", SandboxID: "sbx_1", ProcessID: processID,
			StreamEpoch: 1, Action: streamtoken.ActionAttach,
			IssuedAt: now, NotBefore: now, ExpiresAt: now.Add(time.Minute),
		})
		return tok
	}
	h.stdin = strings.NewReader("hello over the wire\n")
	h.login()

	out, errb, err := h.run("exec", "sbx_1", "--", "cat")
	if err != nil {
		t.Fatalf("exec stream: %v (stderr %s)", err, errb)
	}
	if !strings.Contains(out, "hello over the wire") {
		t.Fatalf("streamed stdout missing; got %q / stderr %q", out, errb)
	}
}

func TestIdempotencyKeyGeneratedUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		k := newIdempotencyKey()
		if !strings.HasPrefix(k, "ictl-") || seen[k] {
			t.Fatalf("bad or duplicate key: %q", k)
		}
		seen[k] = true
	}
}
