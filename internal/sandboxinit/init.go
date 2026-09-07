package sandboxinit

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

const DefaultListenAddr = ":8081"

type GPUDetector func() (string, error)

type Supervisor struct {
	detectGPU GPUDetector
	mu        sync.Mutex
	gpuUUID   string
	procs     *ProcessManager
}

func New(detector GPUDetector) *Supervisor {
	if detector == nil {
		detector = detectAssignedGPU
	}
	return &Supervisor{detectGPU: detector}
}

// WithProcessManager attaches the tenant-process supervisor so GET /v1/processes
// reports observed state.
func (s *Supervisor) WithProcessManager(pm *ProcessManager) *Supervisor {
	s.procs = pm
	return s
}

func (s *Supervisor) checkReady() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gpuUUID != "" {
		return s.gpuUUID, nil
	}
	uuid, err := s.detectGPU()
	if err != nil {
		return "", err
	}
	s.gpuUUID = uuid
	return uuid, nil
}

func (s *Supervisor) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		uuid, err := s.checkReady()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if err != nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		if uuid != "" {
			w.Header().Set("X-Ignition-GPU-UUID", uuid)
			w.Header().Set("X-Ignition-GPU-Health", "ok")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))
	})
	// Observed tenant-process state for ignition-controller. Reachable only on
	// the Pod network; there is no tenant-facing route to it.
	mux.HandleFunc("GET /v1/processes", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		out := map[string]observed{}
		if s.procs != nil {
			out = s.procs.Observed()
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"processes": out})
	})
	// Exec byte stream. ignition-gateway proxies a client WebSocket here after
	// validating the exec-stream token.
	mux.HandleFunc("GET /v1/processes/{process}/attach", s.attach)
	return mux
}

// detectAssignedGPU is the readiness probe for GPU sandboxes. It never consults
// NVIDIA_VISIBLE_DEVICES or a device-node name: it stats the device nodes, runs
// nvidia-smi for a canonical UUID and ECC health, and runs the cuda-check
// helper for a real cuInit(). The authoritative identity plus the
// residual-process verdict still come from ignition-gpu-agent via Pod
// annotations the controller gates on.
func detectAssignedGPU() (string, error) {
	return defaultGPUProbe().run(context.Background())
}

// detectNoAccelerator is the readiness probe for CPU-only sandboxes: the
// supervisor being up is the entire readiness signal.
func detectNoAccelerator() (string, error) { return "", nil }

// Run serves kubelet probes, the controller-only process API, and the exec
// byte stream until SIGTERM/SIGINT, and drives the tenant-process supervisor.
func Run() error {
	var detector GPUDetector
	if strings.EqualFold(strings.TrimSpace(os.Getenv("IGNITION_ACCELERATOR")), "NONE") {
		detector = detectNoAccelerator
	}
	pm := NewProcessManager(os.Getenv("IGNITION_PROC_DESIRED_FILE"), os.Getenv("IGNITION_PROC_WORKDIR"))
	sup := New(detector).WithProcessManager(pm)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pmStop := make(chan struct{})
	go pm.Run(pmStop, time.Second)
	defer close(pmStop)

	srv := &http.Server{
		Addr:              DefaultListenAddr,
		Handler:           sup.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe() }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
