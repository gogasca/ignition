package capacity

import (
	"sort"
	"time"
)

// Inputs are the signals for the warm-node control loop.
type Inputs struct {
	CreatePerMinute  float64
	NodeProvisionMin float64
	Busy             int
	Warm             int
	Queued           int
	MinWarm          int
	MaxWarm          int
	MaxNodes         int
	Safety           float64
}

// DesiredWarm is idle GPU nodes to hold for the 9s SLO.
func DesiredWarm(in Inputs) int {
	if in.Safety <= 0 {
		in.Safety = 1.3
	}
	need := int(in.CreatePerMinute*in.NodeProvisionMin*in.Safety + 0.999)
	if need < in.MinWarm {
		need = in.MinWarm
	}
	if in.MaxWarm > 0 && need > in.MaxWarm {
		need = in.MaxWarm
	}
	return need
}

// DesiredNodes is busy + warm + queued, capped by quota.
func DesiredNodes(in Inputs) int {
	n := in.Busy + DesiredWarm(in) + in.Queued
	if n < in.MinWarm {
		n = in.MinWarm
	}
	if in.MaxNodes > 0 && n > in.MaxNodes {
		n = in.MaxNodes
	}
	return n
}

// P95CreatesPerMinute returns the nearest-rank p95 of per-minute create counts
// in the rolling window ending at now. Empty minutes are included: a burst in
// an otherwise idle window must not be mistaken for a sustained average, while
// a consistently busy window raises the warm target. The current partial
// minute is included as the newest bucket.
func P95CreatesPerMinute(created []time.Time, now time.Time, window time.Duration) float64 {
	if window <= 0 {
		return 0
	}
	buckets := int((window + time.Minute - 1) / time.Minute)
	if buckets < 1 {
		buckets = 1
	}
	counts := make([]int, buckets)
	cutoff := now.Add(-window)
	for _, at := range created {
		if at.After(now) || at.Before(cutoff) {
			continue
		}
		age := int(now.Sub(at) / time.Minute)
		if age >= buckets {
			age = buckets - 1
		}
		counts[age]++
	}
	sort.Ints(counts)
	// Nearest-rank percentile: ceil(0.95*n), converted to a zero-based index.
	idx := (95*buckets + 99) / 100
	if idx < 1 {
		idx = 1
	}
	return float64(counts[idx-1])
}
