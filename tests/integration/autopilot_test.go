package integration_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"ignition.dev/ignition/internal/k8s"
	"ignition.dev/ignition/internal/probe"
)

// newProbeClient returns a probe.Client wired to this world, authenticating as
// alice (owner) with a tiny poll interval so lifecycle journeys finish fast.
func newProbeClient(w *world) *probe.Client {
	return probe.New(w.ts.URL, "prj_dev",
		probe.WithStaticToken("alice"),
		probe.WithPollInterval(10*time.Millisecond))
}

// startAutopilot runs the controller reconcile loop plus a goroutine that plays
// the part of the kubelet + sandbox-init: it walks the fake cluster every few
// milliseconds and advances each pod one phase, and marks desired processes
// RUNNING (then EXITED once cancellation is requested). It stops on t.Cleanup.
//
// This lets the probe journeys run their real polling loops against an
// in-process API+controller, exactly as they would against live staging.
func startAutopilot(t *testing.T, w *world) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() { _ = w.ctrl.Loop(ctx, 5*time.Millisecond) }()

	go func() {
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		procSeen := map[string]bool{}
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			pods, _ := w.fake.List()
			for i := range pods {
				p := pods[i]
				switch {
				case !p.Scheduled:
					w.fake.SetScheduled(p.Name, "gke-node-1")
				case p.Scheduled && !p.Running:
					w.fake.SetRunning(p.Name)
				case p.Running && !p.Ready:
					w.fake.SetReady(p.Name, "GPU-"+p.Name)
				}
				driveProcesses(w, p, procSeen)
			}
		}
	}()
}

func driveProcesses(w *world, p k8s.Pod, seen map[string]bool) {
	raw := p.Annotations[k8s.AnnotProcDesired]
	if raw == "" {
		return
	}
	var desired map[string]struct {
		Cancel bool   `json:"cancel"`
		Signal string `json:"signal"`
	}
	if json.Unmarshal([]byte(raw), &desired) != nil {
		return
	}
	for id, d := range desired {
		key := p.Name + "/" + id
		if d.Cancel || d.Signal != "" {
			w.fake.SetProcessObserved(p.Name, id, "EXITED", intPtr(0))
			continue
		}
		if !seen[key] {
			w.fake.SetProcessObserved(p.Name, id, "RUNNING", nil)
			seen[key] = true
		}
	}
}

func intPtr(i int) *int { return &i }
