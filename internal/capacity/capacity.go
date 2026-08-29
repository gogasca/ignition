package capacity

// Inputs are the signals for the warm-node control loop.
type Inputs struct {
	CreatePerMinute   float64
	NodeProvisionMin  float64
	Busy              int
	Warm              int
	Queued            int
	MinWarm           int
	MaxWarm           int
	MaxNodes          int
	Safety            float64
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
