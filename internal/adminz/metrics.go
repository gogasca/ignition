package adminz

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// HTTPMetrics instruments an HTTP handler. Collectors register into a
// caller-supplied registry so there is no global state.
type HTTPMetrics struct {
	rec      *Recorder
	requests *prometheus.CounterVec
	latency  *prometheus.HistogramVec
	inFlight prometheus.Gauge
	authRej  prometheus.Counter
}

// NewHTTPMetrics registers HTTP collectors into reg and feeds the same events
// into rec (which powers /statusz and /rpcz).
func NewHTTPMetrics(reg prometheus.Registerer, rec *Recorder) *HTTPMetrics {
	m := &HTTPMetrics{
		rec: rec,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ignition_http_requests_total",
			Help: "HTTP requests by method, route pattern and result code.",
		}, []string{"method", "route", "code"}),
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "ignition_http_request_duration_seconds",
			Help:    "HTTP request latency by method and route pattern.",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		}, []string{"method", "route"}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ignition_http_requests_in_flight",
			Help: "HTTP requests currently being served.",
		}),
		authRej: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ignition_http_auth_rejected_total",
			Help: "Requests rejected by authentication before routing.",
		}),
	}
	reg.MustRegister(m.requests, m.latency, m.inFlight, m.authRej)
	return m
}

// AuthRejected records a request the auth layer refused before routing.
func (m *HTTPMetrics) AuthRejected() { m.authRej.Inc() }

// Middleware wraps next (which must be the ServeMux, so r.Pattern is populated
// after it runs) with timing, status capture, Prometheus, and the ring recorder.
func (m *HTTPMetrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.inFlight.Inc()
		start := time.Now()
		sr := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(sr, r)

		dur := time.Since(start)
		m.inFlight.Dec()

		route := r.Pattern
		if route == "" {
			route = "(unmatched)"
		}
		code := sr.Header().Get("X-Ignition-Error-Code")
		if code == "" {
			if sr.status >= 400 {
				code = "HTTP_" + strconv.Itoa(sr.status)
			} else {
				code = "OK"
			}
		}
		m.requests.WithLabelValues(r.Method, route, code).Inc()
		m.latency.WithLabelValues(r.Method, route).Observe(dur.Seconds())
		if m.rec != nil {
			m.rec.Add(Event{
				Time:      start,
				Kind:      "http",
				Method:    r.Method,
				Route:     route,
				Status:    sr.status,
				Code:      code,
				Dur:       dur,
				RequestID: w.Header().Get("X-Request-Id"),
			})
		}
	})
}

// statusRecorder captures the response status code.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.written {
		s.status = code
		s.written = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.written = true
	return s.ResponseWriter.Write(b)
}

// Flush proxies http.Flusher for the SSE (:watch) handlers.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ReconcileMetrics instruments the controller reconcile loop.
type ReconcileMetrics struct {
	rec       *Recorder
	total     *prometheus.CounterVec
	duration  prometheus.Histogram
	lastTS    prometheus.Gauge
	consecErr prometheus.Gauge
	sandboxes *prometheus.GaugeVec
	leaseHeld prometheus.Gauge
}

// NewReconcileMetrics registers controller collectors into reg.
func NewReconcileMetrics(reg prometheus.Registerer, rec *Recorder) *ReconcileMetrics {
	m := &ReconcileMetrics{
		rec: rec,
		total: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ignition_reconcile_total",
			Help: "Reconcile passes by result.",
		}, []string{"result"}),
		duration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "ignition_reconcile_duration_seconds",
			Help:    "Reconcile pass wall time.",
			Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}),
		lastTS: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ignition_reconcile_last_timestamp_seconds",
			Help: "Unix time of the last completed reconcile pass.",
		}),
		consecErr: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ignition_reconcile_consecutive_errors",
			Help: "Consecutive failing reconcile passes.",
		}),
		sandboxes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ignition_sandboxes",
			Help: "Sandboxes known to the controller, by state.",
		}, []string{"state"}),
		leaseHeld: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ignition_controller_lease_held",
			Help: "1 when this replica holds the reconcile lease.",
		}),
	}
	reg.MustRegister(m.total, m.duration, m.lastTS, m.consecErr, m.sandboxes, m.leaseHeld)
	return m
}

// ObservePass records the outcome of one reconcile pass.
func (m *ReconcileMetrics) ObservePass(dur time.Duration, err error, byState map[string]int, leaseHeld bool, consecErrs int) {
	result := "ok"
	if err != nil {
		result = "error"
	}
	m.total.WithLabelValues(result).Inc()
	m.duration.Observe(dur.Seconds())
	m.lastTS.SetToCurrentTime()
	m.consecErr.Set(float64(consecErrs))
	for state, n := range byState {
		m.sandboxes.WithLabelValues(state).Set(float64(n))
	}
	if leaseHeld {
		m.leaseHeld.Set(1)
	} else {
		m.leaseHeld.Set(0)
	}
	if m.rec != nil {
		ev := Event{Time: time.Now(), Kind: "reconcile", Route: "reconcile", Dur: dur, Code: "OK"}
		if err != nil {
			ev.Code = "ERROR"
			ev.Err = strings.TrimSpace(err.Error())
		}
		m.rec.Add(ev)
	}
}
