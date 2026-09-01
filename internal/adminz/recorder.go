package adminz

import (
	"sort"
	"sync"
	"time"
)

// Event is one recorded HTTP request or reconcile pass.
type Event struct {
	Time      time.Time
	Kind      string // "http" | "reconcile"
	Method    string // GET/POST, empty for reconcile
	Route     string // ServeMux pattern, or "reconcile"
	Status    int    // HTTP status, 0 for reconcile
	Code      string // X-Ignition-Error-Code, or "OK" / "ERROR"
	Dur       time.Duration
	RequestID string
	Err       string // trimmed error text (reconcile / 5xx)
}

// Recorder keeps the most recent Events in a fixed-size ring. Safe for
// concurrent use.
type Recorder struct {
	mu   sync.Mutex
	buf  []Event
	next int
	n    int
}

// NewRecorder returns a Recorder holding up to capacity events (min 1).
func NewRecorder(capacity int) *Recorder {
	if capacity < 1 {
		capacity = 200
	}
	return &Recorder{buf: make([]Event, capacity)}
}

// Add records an event, evicting the oldest when full.
func (r *Recorder) Add(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.next] = e
	r.next = (r.next + 1) % len(r.buf)
	if r.n < len(r.buf) {
		r.n++
	}
}

// Snapshot returns the recorded events, newest first.
func (r *Recorder) Snapshot() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Event, 0, r.n)
	for i := 0; i < r.n; i++ {
		idx := (r.next - 1 - i + len(r.buf)*2) % len(r.buf)
		out = append(out, r.buf[idx])
	}
	return out
}

// RouteStat aggregates the events currently in the ring for one route.
type RouteStat struct {
	Route  string
	Count  int
	Errors int // Status >= 400, or non-empty Err
	P50    time.Duration
	P90    time.Duration
	P99    time.Duration
	Max    time.Duration
}

// Aggregate summarises the ring per route. Percentiles are over the retained
// window only — /metrics carries the full-history histogram.
func (r *Recorder) Aggregate() []RouteStat {
	events := r.Snapshot()
	byRoute := map[string][]time.Duration{}
	errs := map[string]int{}
	for _, e := range events {
		route := e.Route
		if route == "" {
			route = "(unmatched)"
		}
		byRoute[route] = append(byRoute[route], e.Dur)
		if e.Status >= 400 || e.Err != "" {
			errs[route]++
		}
	}
	out := make([]RouteStat, 0, len(byRoute))
	for route, ds := range byRoute {
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		out = append(out, RouteStat{
			Route:  route,
			Count:  len(ds),
			Errors: errs[route],
			P50:    percentile(ds, 0.50),
			P90:    percentile(ds, 0.90),
			P99:    percentile(ds, 0.99),
			Max:    ds[len(ds)-1],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Route < out[j].Route })
	return out
}

// percentile returns the p-quantile of a sorted, non-empty slice (nearest rank).
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p * float64(len(sorted)))
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
