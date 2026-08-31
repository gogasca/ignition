package integration_test

import (
	"context"
	"testing"
	"time"

	"ignition.dev/ignition/internal/probe"
)

// TestProbeJourneys runs every prober critical-user-journey against an
// in-process API + controller + auto-driven fake cluster. This exercises the
// exact code path cmd/ignition-prober runs against live staging.
func TestProbeJourneys(t *testing.T) {
	w := newWorld(t)
	startAutopilot(t, w)
	c := newProbeClient(w)
	env := probe.Env{Project: "prj_dev", ImageID: "img_seed"}

	for _, j := range probe.All() {
		j := j
		t.Run(j.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			res := probe.Run(ctx, c, []probe.Journey{j}, env)[0]
			if !res.OK {
				t.Fatalf("journey %s failed after %s: %v\nsteps: %+v", j.Name, res.Dur, res.Err, res.Steps)
			}
			if len(res.Steps) == 0 {
				t.Fatalf("journey %s recorded no steps", j.Name)
			}
		})
	}
}

// TestProbeSelectLite confirms the read-only subset runs without a controller.
func TestProbeSelectLite(t *testing.T) {
	w := newWorld(t)
	c := newProbeClient(w)
	lite, err := probe.Select("lite")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, r := range probe.Run(ctx, c, lite, probe.Env{Project: "prj_dev", ImageID: "img_seed"}) {
		if !r.OK {
			t.Fatalf("lite journey %s failed: %v", r.Journey, r.Err)
		}
	}
}
