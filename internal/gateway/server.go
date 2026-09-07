// Package gateway is ignition-gateway: the exec byte-stream data plane. It
// validates the short-lived exec-stream token minted by ignition-api, resolves
// the target sandbox to a Pod address, and proxies a client WebSocket to that
// sandbox's init supervisor. It never parses exec frames — they pass through
// opaquely — and it holds no product database access.
package gateway

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"ignition.dev/ignition/internal/config"
	"ignition.dev/ignition/internal/k8s"
	"ignition.dev/ignition/internal/streamtoken"
)

const sandboxSupervisorPort = 8081

// Endpoint is a resolved sandbox backend.
type Endpoint struct {
	PodIP      string
	Ready      bool
	Generation int64
}

// Resolver maps a sandbox to its current Pod endpoint.
type Resolver interface {
	Resolve(ctx context.Context, projectID, sandboxID string) (Endpoint, error)
}

// Server proxies attach WebSockets.
type Server struct {
	secret   string
	audience string
	resolver Resolver

	upgrader websocket.Upgrader
	dialer   *websocket.Dialer
}

func New(secret, audience string, resolver Resolver) *Server {
	return &Server{
		secret:   secret,
		audience: audience,
		resolver: resolver,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  32 << 10,
			WriteBufferSize: 32 << 10,
			CheckOrigin:     func(*http.Request) bool { return true },
		},
		dialer: &websocket.Dialer{HandshakeTimeout: 10 * time.Second},
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /v1/attach", s.attach)
	return mux
}

func tokenFromRequest(r *http.Request) string {
	if t := strings.TrimSpace(r.URL.Query().Get("token")); t != "" {
		return t
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	return ""
}

func (s *Server) attach(w http.ResponseWriter, r *http.Request) {
	raw := tokenFromRequest(r)
	if raw == "" {
		http.Error(w, "missing stream token", http.StatusUnauthorized)
		return
	}
	claims, err := streamtoken.Verify(s.secret, raw, s.audience)
	if err != nil {
		http.Error(w, "invalid stream token", http.StatusUnauthorized)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	ep, err := s.resolver.Resolve(ctx, claims.ProjectID, claims.SandboxID)
	cancel()
	if err != nil || ep.PodIP == "" {
		http.Error(w, "sandbox is not reachable", http.StatusConflict)
		return
	}
	if !ep.Ready {
		http.Error(w, "sandbox is not READY", http.StatusConflict)
		return
	}
	if claims.Generation != 0 && ep.Generation != 0 && claims.Generation != ep.Generation {
		http.Error(w, "stale sandbox generation", http.StatusConflict)
		return
	}

	authority := ep.PodIP
	if !strings.Contains(authority, ":") {
		authority += ":" + strconv.Itoa(sandboxSupervisorPort)
	}
	backendURL := "ws://" + authority + "/v1/processes/" + claims.ProcessID + "/attach"
	backend, resp, err := s.dialer.Dial(backendURL, nil)
	if err != nil {
		code := http.StatusBadGateway
		if resp != nil {
			code = resp.StatusCode
		}
		http.Error(w, "cannot reach sandbox supervisor", code)
		return
	}
	defer backend.Close()

	client, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer client.Close()

	proxy(client, backend)
}

// proxy copies WebSocket messages both ways until either side closes.
func proxy(a, b *websocket.Conn) {
	done := make(chan struct{}, 2)
	go copyConn(a, b, done)
	go copyConn(b, a, done)
	<-done
}

func copyConn(dst, src *websocket.Conn, done chan<- struct{}) {
	defer func() { done <- struct{}{} }()
	for {
		mt, msg, err := src.ReadMessage()
		if err != nil {
			_ = dst.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
				time.Now().Add(5*time.Second))
			return
		}
		_ = dst.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err := dst.WriteMessage(mt, msg); err != nil {
			return
		}
	}
}

// Run serves the gateway against GKE-resolved sandbox endpoints.
func Run(cfg config.Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.StreamTokenSecret) == "" {
		return errors.New("ignition-gateway requires IGNITION_STREAM_TOKEN_SECRET")
	}
	restCfg, err := k8s.RESTConfig(cfg.KubeconfigPath)
	if err != nil {
		return err
	}
	resolver, err := NewK8sResolver(restCfg)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           New(cfg.StreamTokenSecret, cfg.GatewayURL, resolver).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("ignition-gateway: listening on %s (audience %s)", cfg.ListenAddr, cfg.GatewayURL)
	return srv.ListenAndServe()
}
