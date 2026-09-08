package probe

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"ignition.dev/ignition/internal/execframe"
)

var wsUpgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

// gatewayStub answers the attach WebSocket like ignition-gateway + sandbox-init:
// it writes the given frames then closes normally.
func gatewayStub(t *testing.T, frames []execframe.Frame) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") == "" {
			http.Error(w, "missing token", http.StatusUnauthorized)
			return
		}
		c, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for _, f := range frames {
			if err := c.WriteJSON(f); err != nil {
				return
			}
		}
		_ = c.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			time.Now().Add(time.Second))
	}
}

func TestStreamAttachReadsStdoutThenExit(t *testing.T) {
	code := 0
	ts := httptest.NewServer(gatewayStub(t, []execframe.Frame{
		{Channel: execframe.Stdout, Kind: execframe.Data, Payload: []byte("nonce-abc\n")},
		{Channel: execframe.Control, Kind: execframe.Exit, ExitCode: &code},
	}))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := streamAttach(ctx, ts.URL, "tok")
	if err != nil {
		t.Fatalf("streamAttach: %v", err)
	}
	if !res.SawStdout || !strings.Contains(res.Stdout, "nonce-abc") {
		t.Fatalf("stdout = %q", res.Stdout)
	}
	if !res.SawExit || res.ExitCode == nil || *res.ExitCode != 0 {
		t.Fatalf("exit = %v %v", res.SawExit, res.ExitCode)
	}
}

func TestStreamAttachExitOnly(t *testing.T) {
	// A shell-less image: sandbox-init frames only the terminal control frame.
	ts := httptest.NewServer(gatewayStub(t, []execframe.Frame{
		{Channel: execframe.Control, Kind: execframe.Exit},
	}))
	defer ts.Close()
	res, err := streamAttach(context.Background(), ts.URL, "tok")
	if err != nil {
		t.Fatalf("streamAttach: %v", err)
	}
	if res.SawStdout || !res.SawExit {
		t.Fatalf("res = %+v", res)
	}
}

func TestStreamAttachErrorFrame(t *testing.T) {
	ts := httptest.NewServer(gatewayStub(t, []execframe.Frame{
		{Kind: execframe.Error, Reason: "backend gone"},
	}))
	defer ts.Close()
	if _, err := streamAttach(context.Background(), ts.URL, "tok"); err == nil ||
		!strings.Contains(err.Error(), "backend gone") {
		t.Fatalf("want error frame surfaced, got %v", err)
	}
}

func TestStreamAttachHandshakeFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusConflict) // gateway 409: not READY / stale generation
	}))
	defer ts.Close()
	if _, err := streamAttach(context.Background(), ts.URL, "tok"); err == nil ||
		!strings.Contains(err.Error(), "handshake") {
		t.Fatalf("want handshake error, got %v", err)
	}
}

func TestStreamAttachBadScheme(t *testing.T) {
	if _, err := streamAttach(context.Background(), "ftp://x", "tok"); err == nil {
		t.Fatal("want error for non-http scheme")
	}
}

// TestJourneyGatewayExec drives the whole journey against one stub acting as
// both the API and the gateway. The journey's nonce is internal and random, so
// the stub streams only the terminal frame — the shell-less-image path, which
// still exercises create → ready → attach → real WebSocket round trip → close →
// terminate.
func TestJourneyGatewayExec(t *testing.T) {
	var terminated atomic.Bool

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/attach":
			gatewayStub(t, []execframe.Frame{{Channel: execframe.Control, Kind: execframe.Exit}})(w, r)

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sandboxes"):
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"sandbox":{"id":"sbx_1","state":"CREATING"},"operation":{"id":"op_1"}}`))

		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/sandboxes/sbx_1"):
			if terminated.Load() {
				_, _ = w.Write([]byte(`{"id":"sbx_1","state":"FINISHED"}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"sbx_1","state":"READY","readyTime":"2026-01-01T00:00:00Z"}`))

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/processes"):
			_, _ = w.Write([]byte(`{"id":"prc_1","state":"RUNNING"}`))

		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, ":attach"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"streamToken": "stream.tok", "gatewayUrl": "http://" + r.Host,
			})

		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, ":terminate"):
			terminated.Store(true)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"sandbox":{"id":"sbx_1","state":"TERMINATING"},"operation":{}}`))

		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	defer ts.Close()

	c := New(ts.URL, "prj_dev", WithStaticToken("tok"), WithPollInterval(time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res := Run(ctx, c, []Journey{journeyGatewayExec}, Env{Project: "prj_dev", ImageID: "img_seed"})[0]
	if !res.OK {
		t.Fatalf("gateway-exec failed: %v\nsteps: %+v", res.Err, res.Steps)
	}
	stepNames := make([]string, len(res.Steps))
	for i, s := range res.Steps {
		stepNames[i] = s.Name
	}
	if got := strings.Join(stepNames, ","); got != "create-sandbox,wait-ready,create-process,attach-stream,terminate" {
		t.Fatalf("steps = %s", got)
	}
}

func TestJourneyGatewayExecFailsOnDeadGateway(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/attach":
			http.Error(w, "not reachable", http.StatusConflict)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sandboxes"):
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"sandbox":{"id":"sbx_1","state":"CREATING"},"operation":{}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/sandboxes/sbx_1"):
			_, _ = w.Write([]byte(`{"id":"sbx_1","state":"READY","readyTime":"2026-01-01T00:00:00Z"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/processes"):
			_, _ = w.Write([]byte(`{"id":"prc_1","state":"RUNNING"}`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, ":attach"):
			_ = json.NewEncoder(w).Encode(map[string]any{"streamToken": "t", "gatewayUrl": "http://" + r.Host})
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, ":terminate"):
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"sandbox":{},"operation":{}}`))
		}
	}))
	defer ts.Close()

	c := New(ts.URL, "prj_dev", WithStaticToken("tok"), WithPollInterval(time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res := Run(ctx, c, []Journey{journeyGatewayExec}, Env{Project: "prj_dev", ImageID: "img_seed"})[0]
	if res.OK {
		t.Fatal("gateway-exec passed against an unreachable gateway")
	}
	if !strings.Contains(res.Err.Error(), "attach-stream") {
		t.Fatalf("want failure at attach-stream, got %v", res.Err)
	}
}
