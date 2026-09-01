package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"ignition.dev/ignition/internal/adminz"
	"ignition.dev/ignition/internal/auth"
	"ignition.dev/ignition/internal/config"
	"ignition.dev/ignition/internal/store"
)

type ctxKey int

const (
	ctxPrincipal ctxKey = iota
	ctxRequestID
)

// Server is the public API.
type Server struct {
	cfg      config.Config
	store    store.Store
	authn    auth.Authenticator
	now      func() time.Time
	started  time.Time
	registry *prometheus.Registry
	recorder *adminz.Recorder
	metrics  *adminz.HTTPMetrics
}

func New(cfg config.Config, st store.Store, authn auth.Authenticator) *Server {
	reg := prometheus.NewRegistry()
	rec := adminz.NewRecorder(200)
	return &Server{
		cfg:      cfg,
		store:    st,
		authn:    authn,
		now:      time.Now,
		started:  time.Now(),
		registry: reg,
		recorder: rec,
		metrics:  adminz.NewHTTPMetrics(reg, rec),
	}
}

// Handler returns the authenticated HTTP handler. It does not listen.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/projects/{project}/sandboxes", s.createSandbox)
	mux.HandleFunc("GET /v1/projects/{project}/sandboxes", s.listSandboxes)
	mux.HandleFunc("GET /v1/projects/{project}/runtimes/default", s.getDefaultRuntime)
	mux.HandleFunc("GET /v1/projects/{project}/sandboxes/{sandbox}", s.getOrWatchSandbox)
	mux.HandleFunc("POST /v1/projects/{project}/sandboxes/{sandbox}", s.postSandbox)
	mux.HandleFunc("POST /v1/projects/{project}/sandboxes/{sandbox}/processes", s.createProcess)
	mux.HandleFunc("GET /v1/projects/{project}/sandboxes/{sandbox}/processes", s.listProcesses)
	mux.HandleFunc("GET /v1/projects/{project}/sandboxes/{sandbox}/processes/{process}", s.getProcess)
	mux.HandleFunc("POST /v1/projects/{project}/sandboxes/{sandbox}/processes/{process}", s.postProcess)
	mux.HandleFunc("GET /v1/projects/{project}/operations", s.listOperations)
	mux.HandleFunc("GET /v1/projects/{project}/operations/{operation}", s.getOrWatchOperation)
	mux.HandleFunc("POST /v1/projects/{project}/operations/{operation}", s.postOperation)
	mux.HandleFunc("GET /healthz", s.healthz)
	// authMiddleware (outer) → metrics middleware (records the matched route) → mux.
	return s.authMiddleware(s.metrics.Middleware(mux))
}

// Go ServeMux wildcards must be a full path segment, so AIP custom methods
// (`{id}:terminate`) are dispatched by splitting the last `:` on the segment.
func splitCustom(raw string) (id, method string) {
	i := strings.LastIndex(raw, ":")
	if i <= 0 {
		return raw, ""
	}
	return raw[:i], raw[i+1:]
}

func (s *Server) getOrWatchSandbox(w http.ResponseWriter, r *http.Request) {
	id, method := splitCustom(r.PathValue("sandbox"))
	r.SetPathValue("sandbox", id)
	switch method {
	case "":
		s.getSandbox(w, r)
	case "watch":
		s.watchSandbox(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) postSandbox(w http.ResponseWriter, r *http.Request) {
	id, method := splitCustom(r.PathValue("sandbox"))
	r.SetPathValue("sandbox", id)
	if method != "terminate" {
		http.NotFound(w, r)
		return
	}
	s.terminateSandbox(w, r)
}

func (s *Server) postProcess(w http.ResponseWriter, r *http.Request) {
	id, method := splitCustom(r.PathValue("process"))
	r.SetPathValue("process", id)
	switch method {
	case "attach":
		s.attachProcess(w, r)
	case "signal":
		s.signalProcess(w, r)
	case "cancel":
		s.cancelProcess(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) getOrWatchOperation(w http.ResponseWriter, r *http.Request) {
	id, method := splitCustom(r.PathValue("operation"))
	r.SetPathValue("operation", id)
	switch method {
	case "":
		s.getOperation(w, r)
	case "watch":
		s.watchOperation(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) postOperation(w http.ResponseWriter, r *http.Request) {
	id, method := splitCustom(r.PathValue("operation"))
	r.SetPathValue("operation", id)
	if method != "cancel" {
		http.NotFound(w, r)
		return
	}
	s.cancelOperation(w, r)
}

// Run serves the public v1 REST API. It must not call the Kubernetes API.
func Run(cfg config.Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	st, _, closer, err := store.Open(context.Background(), cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer closer()
	if cfg.DatabaseURL != "" {
		log.Printf("ignition-api: using postgres (Cloud SQL)")
	} else {
		log.Printf("ignition-api: using in-memory store (set DATABASE_URL)")
	}

	dev := strings.TrimSpace(cfg.DevBearer)
	var authn auth.Authenticator
	switch {
	case dev != "":
		authn = auth.Static{Tokens: map[string]auth.Principal{dev: {Subject: "dev"}}}
		if seeder, ok := st.(store.DevSeeder); ok {
			seeder.SeedRole("prj_dev", "dev", auth.RoleOwner)
			seeder.SeedImage("prj_dev", "img_seed")
		}
		log.Printf("ignition-api: DEV bearer enabled for subject=dev project=prj_dev")
	case cfg.OIDCIssuer != "":
		authn = &auth.JWT{
			Issuer:    cfg.OIDCIssuer,
			Audience:  cfg.OIDCAudience,
			JWKSURL:   cfg.OIDCJWKSURL,
			Algorithm: "RS256",
		}
	default:
		authn = auth.Static{Tokens: map[string]auth.Principal{}}
	}

	s := New(cfg, st, authn)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	errc := make(chan error, 2)
	go func() {
		log.Printf("ignition-api: listening on %s", cfg.ListenAddr)
		errc <- srv.ListenAndServe()
	}()
	go func() {
		log.Printf("ignition-api: admin listening on %s", cfg.AdminAddr)
		errc <- adminz.Serve(ctx, cfg.AdminAddr, s.AdminHandler())
	}()

	select {
	case err := <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) principal(ctx context.Context) auth.Principal {
	p, _ := ctx.Value(ctxPrincipal).(auth.Principal)
	return p
}

func (s *Server) requestID(ctx context.Context) string {
	id, _ := ctx.Value(ctxRequestID).(string)
	return id
}
