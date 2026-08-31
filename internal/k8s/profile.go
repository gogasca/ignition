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
	if acceleratorType == "" {
		acceleratorType = store.AcceleratorNVIDIAL4
	}
	p, ok := profiles[acceleratorType]
	return p, ok
}
