package gateway_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"ignition.dev/ignition/internal/execframe"
	"ignition.dev/ignition/internal/gateway"
	"ignition.dev/ignition/internal/sandboxinit"
	"ignition.dev/ignition/internal/streamtoken"
)

type fakeResolver struct {
	ep  gateway.Endpoint
	err error
}

func (f fakeResolver) Resolve(context.Context, string, string) (gateway.Endpoint, error) {
	return f.ep, f.err
}

const secret = "gateway-test-secret"
const audience = "https://gw.test"

func mintToken(t *testing.T, processID string, gen int64) string {
	t.Helper()
	now := time.Now()
	tok, err := streamtoken.Sign(secret, streamtoken.Claims{
		Subject: "alice", Audience: audience, ProjectID: "prj_1", SandboxID: "sbx_1",
		ProcessID: processID, Generation: gen, StreamEpoch: 1, Action: streamtoken.ActionAttach,
		IssuedAt: now, NotBefore: now, ExpiresAt: now.Add(5 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// startSupervisor runs a real sandbox-init supervisor with one process defined
// by desiredJSON, served over httptest, and returns its host:port.
func startSupervisor(t *testing.T, desiredJSON string) string {
	t.Helper()
	dir := t.TempDir()
	df := dir + "/desired"
	if err := writeFile(df, desiredJSON); err != nil {
		t.Fatal(err)
	}
	pm := sandboxinit.NewProcessManager(df, dir+"/work")
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go pm.Run(stop, 20*time.Millisecond)

	sup := sandboxinit.New(func() (string, error) { return "GPU-x", nil }).WithProcessManager(pm)
	srv := httptest.NewServer(sup.Handler())
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestGatewayProxiesStdioAndExit(t *testing.T) {
	backend := startSupervisor(t, `{"prc_1":{"command":["cat"]}}`)

	gw := gateway.New(secret, audience, fakeResolver{ep: gateway.Endpoint{PodIP: backend, Ready: true}})
	gwSrv := httptest.NewServer(gw.Handler())
	defer gwSrv.Close()

	wsURL := "ws" + strings.TrimPrefix(gwSrv.URL, "http") + "/v1/attach?token=" + mintToken(t, "prc_1", 0)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	defer conn.Close()

	// cat echoes stdin to stdout.
	if err := conn.WriteJSON(execframe.Frame{Channel: execframe.Stdin, Kind: execframe.Data, Payload: []byte("ping\n")}); err != nil {
		t.Fatal(err)
	}
	if got := readData(t, conn, execframe.Stdout); got != "ping\n" {
		t.Fatalf("stdout = %q, want %q", got, "ping\n")
	}

	// Close stdin => cat exits 0 => Exit frame.
	if err := conn.WriteJSON(execframe.Frame{Channel: execframe.Stdin, Kind: execframe.EOF}); err != nil {
		t.Fatal(err)
	}
	code := readExit(t, conn)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
}

func TestGatewayRejectsBadToken(t *testing.T) {
	gw := gateway.New(secret, audience, fakeResolver{ep: gateway.Endpoint{PodIP: "10.0.0.1", Ready: true}})
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/attach?token=garbage"
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		t.Fatal("expected dial failure")
	}
	if resp == nil || resp.StatusCode != 401 {
		t.Fatalf("status = %v", resp)
	}
}

func TestGatewayRejectsNotReady(t *testing.T) {
	gw := gateway.New(secret, audience, fakeResolver{ep: gateway.Endpoint{PodIP: "10.0.0.1", Ready: false}})
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/attach?token=" + mintToken(t, "prc_1", 0)
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		t.Fatal("expected dial failure")
	}
	if resp == nil || resp.StatusCode != 409 {
		t.Fatalf("status = %v", resp)
	}
}

func TestGatewayRejectsStaleGeneration(t *testing.T) {
	gw := gateway.New(secret, audience, fakeResolver{ep: gateway.Endpoint{PodIP: "10.0.0.1", Ready: true, Generation: 5}})
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/attach?token=" + mintToken(t, "prc_1", 3)
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		t.Fatal("expected dial failure")
	}
	if resp == nil || resp.StatusCode != 409 {
		t.Fatalf("status = %v", resp)
	}
}
