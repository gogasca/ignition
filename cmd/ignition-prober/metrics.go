package main

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"ignition.dev/ignition/internal/probe"
)

// metrics holds the prometheus collectors for the continuous prober. Kept in
// cmd/ so internal/probe stays free of the prometheus dependency.
type metrics struct {
	runs         *prometheus.CounterVec
	journeySecs  *prometheus.HistogramVec
	stepSecs     *prometheus.HistogramVec
	lastSuccess  *prometheus.GaugeVec
	cycleSeconds prometheus.Histogram
	up           prometheus.Gauge
}

func newMetrics() *metrics {
	return &metrics{
		runs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ignition_probe_journey_runs_total",
			Help: "Journey executions by result.",
		}, []string{"journey", "result"}),
		journeySecs: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "ignition_probe_journey_duration_seconds",
			Help:    "Wall time of each journey.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600},
		}, []string{"journey"}),
		stepSecs: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "ignition_probe_step_duration_seconds",
			Help:    "Wall time of each journey step.",
			Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
		}, []string{"journey", "step"}),
		lastSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ignition_probe_last_success_timestamp_seconds",
			Help: "Unix time of the last successful run of each journey.",
		}, []string{"journey"}),
		cycleSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "ignition_probe_cycle_duration_seconds",
			Help:    "Wall time of a full probe cycle.",
			Buckets: []float64{1, 5, 10, 30, 60, 120, 300, 600, 900},
		}),
		up: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ignition_probe_up",
			Help: "1 when the most recent cycle had no failing journey, else 0.",
		}),
	}
}

func (m *metrics) collectors() []prometheus.Collector {
	return []prometheus.Collector{
		m.runs, m.journeySecs, m.stepSecs, m.lastSuccess, m.cycleSeconds, m.up,
	}
}

func (m *metrics) observe(results []probe.Result) {
	anyFail := false
	now := float64(time.Now().Unix())
	for _, r := range results {
		result := "success"
		if !r.OK {
			result = "failure"
			anyFail = true
		}
		m.runs.WithLabelValues(r.Journey, result).Inc()
		m.journeySecs.WithLabelValues(r.Journey).Observe(r.Dur.Seconds())
		for _, s := range r.Steps {
			m.stepSecs.WithLabelValues(r.Journey, s.Name).Observe(s.Dur.Seconds())
		}
		if r.OK {
			m.lastSuccess.WithLabelValues(r.Journey).Set(now)
		}
	}
	if anyFail {
		m.up.Set(0)
	} else {
		m.up.Set(1)
	}
}
