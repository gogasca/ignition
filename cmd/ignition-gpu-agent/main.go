// Command ignition-gpu-agent is the node-local trusted GPU authority. It runs
// as a DaemonSet on the GPU sandbox node pool: one Pod per node, each owning the
// single GPU on its node. It attests the sandbox Pod scheduled there (canonical
// UUID + health + no residual processes) and verifies the GPU is clean before
// the node returns to the warm pool. See internal/gpuagent.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"ignition.dev/ignition/internal/gpuagent"
	"ignition.dev/ignition/internal/k8s"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "ignition-gpu-agent: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	node := strings.TrimSpace(os.Getenv("IGNITION_NODE_NAME"))
	if node == "" {
		return errors.New("IGNITION_NODE_NAME is required (set it from spec.nodeName via the downward API)")
	}
	listen := envOr("IGNITION_GPUAGENT_LISTEN", ":9103")
	interval := durEnv("IGNITION_GPUAGENT_INTERVAL", 5*time.Second)
	namespace := envOr("IGNITION_K8S_NAMESPACE", k8s.Namespace)

	restCfg, err := k8s.RESTConfig(os.Getenv("KUBECONFIG"))
	if err != nil {
		return err
	}
	restCfg.UserAgent = "ignition-gpu-agent"
	cluster, err := k8s.NewCluster(restCfg, namespace)
	if err != nil {
		return err
	}

	insp := gpuagent.NewSMIInspector(os.Getenv("IGNITION_NVIDIA_SMI"))
	agent := gpuagent.New(node, cluster, cluster, insp)
	m := newMetrics()
	agent.Metrics = m

	reg := prometheus.NewRegistry()
	reg.MustRegister(m.collectors()...)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	ok := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
	mux.HandleFunc("/healthz", ok)
	mux.HandleFunc("/readyz", ok)

	srv := &http.Server{Addr: listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()
	loopErr := make(chan error, 1)
	go func() { loopErr <- agent.Loop(ctx, interval) }()

	log.Printf("ignition-gpu-agent: node=%s listen=%s interval=%s namespace=%s", node, listen, interval, namespace)

	select {
	case err := <-serveErr:
		stop()
		<-loopErr
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case err := <-loopErr:
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func durEnv(key string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return fallback
}
