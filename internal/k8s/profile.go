package k8s

import "ignition.dev/ignition/internal/store"

// Profile is the server-owned scheduling shape for one accelerator class. It is
// the single source of truth for which accelerator types are serviceable.
type Profile struct {
	Accelerator   string
	NodePoolValue string // ignition.io/node-pool label value
	TaintKey      string // node taint key to tolerate; "" if the pool is untainted
	GPUQuantity   string // nvidia.com/gpu request; "" for CPU-only
	AntiAffinity  bool   // require one sandbox per node
}

// Internet-specific pools are intentionally separate from restricted pools so
// the VPC firewall tag is part of the scheduling decision, not just a Pod
// label.
const (
	CPUInternetNodePoolValue = "cpu-sandbox-internet"
	GPUInternetNodePoolValue = "gpu-sandbox-l4-internet"
)

var profiles = map[string]Profile{
	store.AcceleratorNone: {
		Accelerator:   store.AcceleratorNone,
		NodePoolValue: CPUNodePoolValue,
		TaintKey:      "ignition.io/sandbox",
	},
	store.AcceleratorNVIDIAL4: {
		Accelerator:   store.AcceleratorNVIDIAL4,
		NodePoolValue: GPUNodePoolValue,
		TaintKey:      "ignition.io/gpu-sandbox",
		GPUQuantity:   "1",
		AntiAffinity:  true,
	},
}

// ProfileFor returns the scheduling profile for a public accelerator type. An
// empty type is treated as NVIDIA_L4 for rows created before the field existed.
// ok is false when the type has no serviceable profile.
func ProfileFor(acceleratorType string) (Profile, bool) {
	return ProfileForNetwork(acceleratorType, false)
}

// ProfileForNetwork returns the server-owned scheduling profile for an
// accelerator and internet-access mode.
func ProfileForNetwork(acceleratorType string, internetEnabled bool) (Profile, bool) {
	if acceleratorType == "" {
		acceleratorType = store.AcceleratorNVIDIAL4
	}
	p, ok := profiles[acceleratorType]
	if !ok {
		return p, false
	}
	if internetEnabled {
		switch acceleratorType {
		case store.AcceleratorNone:
			p.NodePoolValue = CPUInternetNodePoolValue
		case store.AcceleratorNVIDIAL4:
			p.NodePoolValue = GPUInternetNodePoolValue
		}
	}
	return p, ok
}

// IsGPUProfile reports whether the accelerator type schedules a physical GPU
// (and therefore needs the ignition-gpu-agent's UUID + health attestation
// before its sandbox may become public READY). An unknown type is treated as a
// GPU type: the empty/legacy default is NVIDIA_L4, and failing closed keeps a
// mis-sequenced call from skipping the attestation gate.
func IsGPUProfile(acceleratorType string) bool {
	p, ok := ProfileFor(acceleratorType)
	if !ok {
		return true
	}
	return p.GPUQuantity != ""
}
