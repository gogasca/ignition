package controller

import (
	"sync"
	"time"

	"ignition.dev/ignition/internal/adminz"
)

// ControllerStats is a point-in-time view of the reconcile loop, for /statusz.
type ControllerStats struct {
	HolderID          string
	LeaseHeld         bool
	LastPass          time.Time
	LastDuration      time.Duration
	LastError         string
	ConsecutiveErrors int
	Passes            uint64
	SandboxesByState  map[string]int
	Balloons          int
	MinWarm           int
	MinWarmCPU        int
}

type statsHolder struct {
	mu sync.Mutex
	s  ControllerStats
}

func (h *statsHolder) recordPass(dur time.Duration, err error, byState map[string]int, leaseHeld bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.s.Passes++
	h.s.LastPass = time.Now()
	h.s.LastDuration = dur
	h.s.LeaseHeld = leaseHeld
	h.s.SandboxesByState = byState
	if err != nil {
		h.s.LastError = err.Error()
		h.s.ConsecutiveErrors++
	} else {
		h.s.LastError = ""
		h.s.ConsecutiveErrors = 0
	}
}

func (h *statsHolder) setBalloons(n int) {
	h.mu.Lock()
	h.s.Balloons = n
	h.mu.Unlock()
}

func (h *statsHolder) snapshot() ControllerStats {
	h.mu.Lock()
	defer h.mu.Unlock()
	cp := h.s
	if h.s.SandboxesByState != nil {
		cp.SandboxesByState = make(map[string]int, len(h.s.SandboxesByState))
		for k, v := range h.s.SandboxesByState {
			cp.SandboxesByState[k] = v
		}
	}
	return cp
}

// Stats returns a snapshot of the reconcile loop's status.
func (c *Controller) Stats() ControllerStats {
	s := c.stats.snapshot()
	s.HolderID = c.opts.HolderID
	s.MinWarm = c.opts.MinWarm
	s.MinWarmCPU = c.opts.MinWarmCPU
	return s
}

// StatusSections renders Stats() for the adminz /statusz page.
func (c *Controller) StatusSections() []adminz.Section {
	s := c.Stats()
	lastPass := "never"
	if !s.LastPass.IsZero() {
		lastPass = time.Since(s.LastPass).Round(time.Millisecond).String() + " ago"
	}
	rows := [][2]string{
		{"holder", orDash(s.HolderID)},
		{"lease held", boolStr(s.LeaseHeld)},
		{"passes", u64(s.Passes)},
		{"last pass", lastPass},
		{"last duration", s.LastDuration.Round(time.Microsecond).String()},
		{"consecutive errors", itoa(s.ConsecutiveErrors)},
		{"last error", orDash(s.LastError)},
		{"warm pool", itoa(s.Balloons) + " total / min GPU " + itoa(s.MinWarm) + " / min CPU " + itoa(s.MinWarmCPU)},
	}
	sections := []adminz.Section{{Title: "reconcile", Rows: rows}}
	if len(s.SandboxesByState) > 0 {
		var sb [][2]string
		for _, st := range []string{"CREATING", "SCHEDULED", "STARTED", "READY", "TERMINATING", "FINISHED", "FAILED"} {
			if n, ok := s.SandboxesByState[st]; ok {
				sb = append(sb, [2]string{st, itoa(n)})
			}
		}
		sections = append(sections, adminz.Section{Title: "sandboxes", Rows: sb})
	}
	return sections
}
