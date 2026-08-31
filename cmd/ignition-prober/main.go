// Command ignition-prober runs the Ignition critical-user-journey probes
// against a live API, exporting Prometheus metrics. With IGNITION_PROBE_ONESHOT
// it runs each journey once and exits non-zero on any failure (CI / deploy gate).
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"ignition.dev/ignition/internal/probe"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "ignition-prober: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := probe.Load()
	if err != nil {
		return err
	}

	client, err := newClient(cfg)
	if err != nil {
		return err
	}
	journeys, err := probe.Select(cfg.Journeys)
	if err != nil {
		return err
	}
	env := probe.Env{Project: cfg.Project, ImageID: cfg.ImageID}

	if cfg.OneShot {
		ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
		defer cancel()
		results := probe.Run(ctx, client, journeys, env)
		printResults(results)
		if probe.AnyFailed(results) {
			return errors.New("one or more journeys failed")
		}
		return nil
	}
	return serve(cfg, client, journeys, env)
}

func newClient(cfg probe.Config) (*probe.Client, error) {
	opts := []probe.Option{probe.WithPollInterval(2 * time.Second)}
	switch cfg.Auth {
	case "static":
		opts = append(opts, probe.WithStaticToken(cfg.Token))
	case "gcp-idtoken":
		opts = append(opts, probe.WithTokenFunc(probe.IDTokenSource(nil, cfg.Audience)))
	case "none":
		// no Authorization header
	}
	return probe.New(cfg.Target, cfg.Project, opts...), nil
}

// serve runs the continuous prober: an HTTP server for /metrics and /healthz
// plus a ticker that runs the journeys every cfg.Interval.
func serve(cfg probe.Config, client *probe.Client, journeys []probe.Journey, env probe.Env) error {
	m := newMetrics()
	reg := prometheus.NewRegistry()
	reg.MustRegister(m.collectors()...)

	var lastCycle atomic.Int64 // unix seconds of the last completed cycle

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	// Liveness: the process is up and serving. A slow first cycle (cold sandbox
	// node) must not trip this — otherwise the kubelet restarts the prober
	// before it can produce a signal.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	// Readiness: a probe cycle has completed recently. Being not-ready only
	// removes the pod from Service endpoints; it does not restart it.
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if msg, ok := readyState(lastCycle.Load(), time.Now(), 3*cfg.Interval); !ok {
			http.Error(w, msg, http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})

	srv := &http.Server{Addr: cfg.Listen, Handler: mux}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		log.Printf("ignition-prober: listening on %s target=%s journeys=%s interval=%s auth=%s",
			cfg.Listen, cfg.Target, cfg.Journeys, cfg.Interval, cfg.Auth)
		serveErr <- srv.ListenAndServe()
	}()

	runCycle := func() {
		cycleStart := time.Now()
		cctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
		if n, err := probe.SweepStale(cctx, client, 30*time.Minute); err != nil {
			log.Printf("ignition-prober: sweep stale sandboxes: %v", err)
		} else if n > 0 {
			log.Printf("ignition-prober: swept %d leaked probe sandbox(es)", n)
		}
		results := probe.Run(cctx, client, journeys, env)
		m.observe(results)
		m.cycleSeconds.Observe(time.Since(cycleStart).Seconds())
		lastCycle.Store(time.Now().Unix())
		for _, r := range results {
			logResult(r)
		}
	}

	runCycle() // probe immediately on start
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case err := <-serveErr:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-ticker.C:
			runCycle()
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return srv.Shutdown(shutdownCtx)
		}
	}
}

// readyState reports whether a probe cycle completed recently enough for the
// pod to be considered ready. lastUnix is 0 before the first cycle.
func readyState(lastUnix int64, now time.Time, staleAfter time.Duration) (string, bool) {
	if lastUnix == 0 {
		return "no probe cycle completed yet", false
	}
	if now.Sub(time.Unix(lastUnix, 0)) > staleAfter {
		return "last probe cycle is stale", false
	}
	return "", true
}

func printResults(results []probe.Result) {
	for _, r := range results {
		logResult(r)
	}
}

func logResult(r probe.Result) {
	status := "ok"
	if !r.OK {
		status = "FAIL"
	}
	msg := fmt.Sprintf("probe journey=%s result=%s dur=%s", r.Journey, status, r.Dur.Round(time.Millisecond))
	if len(r.Steps) > 0 {
		msg += " steps="
		for i, s := range r.Steps {
			if i > 0 {
				msg += ","
			}
			msg += fmt.Sprintf("%s:%s", s.Name, s.Dur.Round(time.Millisecond))
		}
	}
	if r.Err != nil {
		msg += fmt.Sprintf(" error=%q", r.Err.Error())
	}
	log.Print(msg)
}
