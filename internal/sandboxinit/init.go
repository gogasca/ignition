package sandboxinit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
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
}

func New(detector GPUDetector) *Supervisor {
	if detector == nil {
		detector = detectAssignedGPU
	}
	return &Supervisor{detectGPU: detector}
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
		w.Header().Set("X-Ignition-GPU-UUID", uuid)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))
	})
	return mux
}

func detectGPUFromEnvironment() (string, error) {
	raw := strings.TrimSpace(os.Getenv("NVIDIA_VISIBLE_DEVICES"))
	if raw == "" || strings.EqualFold(raw, "void") || strings.EqualFold(raw, "none") {
		return "", errors.New("NVIDIA_VISIBLE_DEVICES does not identify a GPU")
	}
	parts := strings.Split(raw, ",")
	if len(parts) != 1 {
		return "", fmt.Errorf("expected exactly one GPU, got %d", len(parts))
	}
	uuid := strings.TrimSpace(parts[0])
	if uuid == "" || strings.EqualFold(uuid, "all") {
		return "", errors.New("GPU assignment must be one explicit device UUID")
	}
	return uuid, nil
}

var gpuDeviceName = regexp.MustCompile(`^nvidia[0-9]+$`)

func detectAssignedGPU() (string, error) {
	if uuid, err := detectGPUFromEnvironment(); err == nil {
		return uuid, nil
	}
	infos, _ := filepath.Glob("/proc/driver/nvidia/gpus/*/information")
	var uuids []string
	for _, path := range infos {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			key, value, ok := strings.Cut(line, ":")
			if ok && strings.EqualFold(strings.TrimSpace(key), "GPU UUID") {
				if uuid := strings.TrimSpace(value); uuid != "" {
					uuids = append(uuids, uuid)
				}
			}
		}
	}
	if len(uuids) == 1 {
		return uuids[0], nil
	}
	if len(uuids) > 1 {
		return "", fmt.Errorf("expected exactly one GPU, got %d", len(uuids))
	}
	devices, _ := filepath.Glob("/dev/nvidia*")
	var assigned []string
	for _, path := range devices {
		name := filepath.Base(path)
		if gpuDeviceName.MatchString(name) {
			assigned = append(assigned, name)
		}
	}
	if len(assigned) != 1 {
		return "", fmt.Errorf("expected exactly one assigned GPU device, got %d", len(assigned))
	}
	return assigned[0], nil
}

// Run serves kubelet probes until SIGTERM/SIGINT. Process supervision is added
// behind the same private listener in the next implementation slice.
// detectNoAccelerator is the readiness probe for CPU-only sandboxes: the
// supervisor being up is the entire readiness signal.
func detectNoAccelerator() (string, error) { return "", nil }

func Run() error {
	var detector GPUDetector
	if strings.EqualFold(strings.TrimSpace(os.Getenv("IGNITION_ACCELERATOR")), "NONE") {
		detector = detectNoAccelerator
	}
	srv := &http.Server{
		Addr:              DefaultListenAddr,
		Handler:           New(detector).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
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
