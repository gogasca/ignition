package main

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"ignition.dev/ignition/internal/probe"
)

func TestMetricsObserve(t *testing.T) {
	m := newMetrics()
	m.observe([]probe.Result{
		{Journey: "health", OK: true, Dur: 50 * time.Millisecond,
			Steps: []probe.Step{{Name: "healthz", Dur: 40 * time.Millisecond}}},
		{Journey: "sandbox-lifecycle", OK: false, Dur: 2 * time.Second,
			Steps: []probe.Step{{Name: "create", Dur: time.Second}}},
	})

	if got := testutil.ToFloat64(m.runs.WithLabelValues("health", "success")); got != 1 {
		t.Fatalf("health success count = %v", got)
	}
	if got := testutil.ToFloat64(m.runs.WithLabelValues("sandbox-lifecycle", "failure")); got != 1 {
		t.Fatalf("lifecycle failure count = %v", got)
	}
	// One failing journey => up == 0.
	if got := testutil.ToFloat64(m.up); got != 0 {
		t.Fatalf("up = %v, want 0", got)
	}
	// Only the successful journey records a last-success timestamp.
	if got := testutil.ToFloat64(m.lastSuccess.WithLabelValues("health")); got == 0 {
		t.Fatal("health last-success not set")
	}

	// A clean cycle flips up back to 1.
	m.observe([]probe.Result{{Journey: "health", OK: true, Dur: time.Millisecond}})
	if got := testutil.ToFloat64(m.up); got != 1 {
		t.Fatalf("up = %v after clean cycle, want 1", got)
	}
}

func TestMetricsHistogramsRegister(t *testing.T) {
	m := newMetrics()
	// All collectors must register without panicking (duplicate/label errors).
	for _, c := range m.collectors() {
		if c == nil {
			t.Fatal("nil collector")
		}
	}
	m.observe([]probe.Result{{Journey: "list", OK: true, Dur: time.Second,
		Steps: []probe.Step{{Name: "list-sandboxes", Dur: 500 * time.Millisecond}}}})
	if c := testutil.CollectAndCount(m.stepSecs); c == 0 {
		t.Fatal("step histogram recorded nothing")
	}
}
