package main

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// metrics implements gpuagent.Metrics. Kept in cmd/ so internal/gpuagent stays
// free of the prometheus dependency.
type metrics struct {
	probeSeconds prometheus.Histogram
	healthy      prometheus.Gauge
	attested     prometheus.Counter
	markedDirty  *prometheus.CounterVec
}

func newMetrics() *metrics {
	return &metrics{
		probeSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "ignition_gpuagent_probe_duration_seconds",
			Help:    "Wall time of one nvidia-smi inventory + compute-process query.",
			Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}),
		healthy: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ignition_gpuagent_gpu_healthy",
			Help: "1 when the node's GPU passed the most recent check, else 0.",
		}),
		attested: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ignition_gpuagent_attested_total",
			Help: "Sandbox Pods stamped with a canonical GPU UUID + init-healthy.",
		}),
		markedDirty: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ignition_gpuagent_node_marked_dirty_total",
			Help: "Times the node was annotated gpu-cleanup=ambiguous, by reason.",
		}, []string{"reason"}),
	}
}

func (m *metrics) collectors() []prometheus.Collector {
	return []prometheus.Collector{m.probeSeconds, m.healthy, m.attested, m.markedDirty}
}

func (m *metrics) ObserveProbe(d time.Duration) { m.probeSeconds.Observe(d.Seconds()) }

func (m *metrics) SetHealthy(ok bool) {
	if ok {
		m.healthy.Set(1)
		return
	}
	m.healthy.Set(0)
}

func (m *metrics) IncAttested() { m.attested.Inc() }

func (m *metrics) IncMarkedDirty(reason string) { m.markedDirty.WithLabelValues(reason).Inc() }
