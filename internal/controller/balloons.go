package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"ignition.dev/ignition/internal/capacity"
	"ignition.dev/ignition/internal/k8s"
	"ignition.dev/ignition/internal/store"
)

// warmClass is one accelerator class's independent warm-buffer target. GPU
// nodes are scarce and quota-limited; CPU nodes are not. An operator may want
// warm CPU capacity without paying for warm GPU capacity, or vice versa, so
// each class scales, cools down, and reports size on its own.
type warmClass struct {
	profile k8s.Profile
	minWarm int
	maxWarm int
}

func (c *Controller) warmClasses() []warmClass {
	gpu, _ := k8s.ProfileFor(store.AcceleratorNVIDIAL4)
	cpu, _ := k8s.ProfileFor(store.AcceleratorNone)
	return []warmClass{
		{profile: gpu, minWarm: c.opts.MinWarm, maxWarm: c.opts.MaxWarm},
		{profile: cpu, minWarm: c.opts.MinWarmCPU, maxWarm: c.opts.MaxWarmCPU},
	}
}

// balloonPrefix names a class's balloon Pods so scale-down never deletes or
// double-counts another class's balloons sharing the reconcile pass.
func balloonPrefix(accel string) string {
	slug := strings.ToLower(strings.ReplaceAll(accel, "_", "-"))
	return "balloon-" + slug + "-"
}

// balloonIndex parses the "<N>" a scale-up assigned after prefix. A name that
// doesn't parse (wrong prefix, or a Pod named some other way) sorts first,
// same as index 0 would; the later strings.HasPrefix check in the scale-down
// loop is what actually protects a non-matching name from being deleted.
func balloonIndex(name, prefix string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(name, prefix))
	if err != nil {
		return -1
	}
	return n
}

func (c *Controller) reconcileBalloons(ctx context.Context, sbs []store.Sandbox) error {
	_ = ctx
	classes := c.warmClasses()
	enabled := false
	for _, cl := range classes {
		if cl.minWarm > 0 || cl.maxWarm > 0 {
			enabled = true
		}
	}
	if !enabled {
		return nil
	}
	pods, err := c.pods.List()
	if err != nil {
		return err
	}
	balloonsByClass := map[string][]k8s.Pod{}
	busyByClass := map[string]int{}
	for _, p := range pods {
		switch p.Labels[k8s.LabelWorkload] {
		case k8s.WorkloadBalloon:
			accel := p.Annotations[k8s.AnnotGPUType]
			balloonsByClass[accel] = append(balloonsByClass[accel], p)
		case k8s.WorkloadSandbox:
			if p.Scheduled {
				accel := p.Annotations[k8s.AnnotGPUType]
				busyByClass[accel]++
			}
		}
	}
	queuedByClass := map[string]int{}
	createdByClass := map[string][]time.Time{}
	for _, sb := range sbs {
		accel := sb.Resources.Accelerator.Type
		if accel == "" {
			accel = store.AcceleratorNVIDIAL4
		}
		createdByClass[accel] = append(createdByClass[accel], sb.CreateTime)
		if sb.State == "CREATING" {
			queuedByClass[accel]++
		}
	}
	if c.balloonExcessSince == nil {
		c.balloonExcessSince = map[string]time.Time{}
	}

	now := c.opts.Now()
	total := 0
	for _, cl := range classes {
		accel := cl.profile.Accelerator
		balloons := balloonsByClass[accel]
		prefix := balloonPrefix(accel)
		// Numeric, not lexicographic: names are "<prefix><N>", and string
		// order puts "...-10" before "...-2". A wrong sort here means
		// scale-down below deletes the wrong Pod (leaving a gap instead of
		// the highest index), and the next scale-up's name — derived from
		// len(balloons) — then collides with a Pod that was never deleted.
		sort.Slice(balloons, func(i, j int) bool {
			return balloonIndex(balloons[i].Name, prefix) < balloonIndex(balloons[j].Name, prefix)
		})
		want := capacity.DesiredWarm(capacity.Inputs{
			CreatePerMinute:  capacity.P95CreatesPerMinute(createdByClass[accel], now, c.opts.WarmWindow),
			NodeProvisionMin: c.opts.NodeProvisionTime.Minutes(),
			Busy:             busyByClass[accel],
			Queued:           queuedByClass[accel],
			Warm:             len(balloons),
			MinWarm:          cl.minWarm,
			MaxWarm:          cl.maxWarm,
			Safety:           1.3,
		})
		if len(balloons) > want {
			if c.balloonExcessSince[accel].IsZero() {
				c.balloonExcessSince[accel] = now
			}
			if c.opts.BalloonCooldown > 0 && now.Sub(c.balloonExcessSince[accel]) < c.opts.BalloonCooldown {
				total += len(balloons)
				continue
			}
		} else {
			c.balloonExcessSince[accel] = time.Time{}
		}
		for len(balloons) < want {
			name := fmt.Sprintf("%s%d", prefix, len(balloons))
			if err := c.pods.Create(k8s.BalloonPod(name, cl.profile)); err != nil && !errors.Is(err, k8s.ErrAlreadyExists) {
				return err
			}
			balloons = append(balloons, k8s.Pod{Name: name})
		}
		for len(balloons) > want {
			last := balloons[len(balloons)-1]
			if !strings.HasPrefix(last.Name, prefix) {
				break
			}
			if err := c.pods.Delete(last.Name); err != nil {
				return err
			}
			balloons = balloons[:len(balloons)-1]
		}
		if len(balloons) <= want {
			c.balloonExcessSince[accel] = time.Time{}
		}
		total += len(balloons)
	}
	c.stats.setBalloons(total)
	return nil
}
