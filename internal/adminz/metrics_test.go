package adminz

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestReconcileMetricsObserveStage(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewReconcileMetrics(reg, NewRecorder(10))

	m.ObserveStage("READY", 3*time.Second)
	m.ObserveStage("READY", 5*time.Second)
	m.ObserveStage("SCHEDULED", 1*time.Second)

	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var readyCount, scheduledCount uint64
	for _, f := range families {
		if f.GetName() != "ignition_sandbox_stage_latency_seconds" {
			continue
		}
		for _, metric := range f.GetMetric() {
			for _, lbl := range metric.GetLabel() {
				if lbl.GetName() != "state" {
					continue
				}
				switch lbl.GetValue() {
				case "READY":
					readyCount = metric.GetHistogram().GetSampleCount()
				case "SCHEDULED":
					scheduledCount = metric.GetHistogram().GetSampleCount()
				}
			}
		}
	}
	if readyCount != 2 {
		t.Fatalf("READY sample count = %d, want 2", readyCount)
	}
	if scheduledCount != 1 {
		t.Fatalf("SCHEDULED sample count = %d, want 1", scheduledCount)
	}
}

func TestReconcileMetricsObserveStageIgnoresNegativeLatency(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewReconcileMetrics(reg, NewRecorder(10))
	m.ObserveStage("READY", -1*time.Second)

	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != "ignition_sandbox_stage_latency_seconds" {
			continue
		}
		for _, metric := range f.GetMetric() {
			if metric.GetHistogram().GetSampleCount() != 0 {
				t.Fatalf("negative latency must not be recorded, got %+v", metric)
			}
		}
	}
}
