package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"ignition.dev/ignition/internal/capacity"
	"ignition.dev/ignition/internal/k8s"
	"ignition.dev/ignition/internal/store"
)

func (c *Controller) reconcileBalloons(ctx context.Context, sbs []store.Sandbox) error {
	_ = ctx
	if c.opts.MinWarm <= 0 && c.opts.MaxWarm <= 0 {
		return nil
	}
	pods, err := c.pods.List()
	if err != nil {
		return err
	}
	var balloons []k8s.Pod
	busy, queued := 0, 0
	for _, p := range pods {
		switch p.Labels[k8s.LabelWorkload] {
		case k8s.WorkloadBalloon:
			balloons = append(balloons, p)
		case k8s.WorkloadSandbox:
			if p.Scheduled {
				busy++
			}
		}
	}
	for _, sb := range sbs {
		if sb.State == "CREATING" {
			queued++
		}
	}
	want := capacity.DesiredWarm(capacity.Inputs{
		Busy:    busy,
		Queued:  queued,
		Warm:    len(balloons),
		MinWarm: c.opts.MinWarm,
		MaxWarm: c.opts.MaxWarm,
		Safety:  1.3,
	})
	sort.Slice(balloons, func(i, j int) bool { return balloons[i].Name < balloons[j].Name })
	if len(balloons) > want {
		now := c.opts.Now()
		if c.balloonExcessSince.IsZero() {
			c.balloonExcessSince = now
		}
		if c.opts.BalloonCooldown > 0 && now.Sub(c.balloonExcessSince) < c.opts.BalloonCooldown {
			return nil
		}
	} else {
		c.balloonExcessSince = time.Time{}
	}
	for len(balloons) < want {
		name := fmt.Sprintf("balloon-%d", len(balloons))
		if err := c.pods.Create(k8s.BalloonPod(name)); err != nil {
			return err
		}
		balloons = append(balloons, k8s.Pod{Name: name})
	}
	for len(balloons) > want {
		last := balloons[len(balloons)-1]
		if !strings.HasPrefix(last.Name, "balloon-") {
			break
		}
		if err := c.pods.Delete(last.Name); err != nil {
			return err
		}
		balloons = balloons[:len(balloons)-1]
	}
	if len(balloons) <= want {
		c.balloonExcessSince = time.Time{}
	}
	return nil
}
