// Package gpuid holds the GPU UUID contract shared by sandbox-init, the
// controller, and ignition-gpu-agent. It is a leaf package with no
// dependencies so sandbox-init stays small.
package gpuid

import "regexp"

// canonical matches the NVML / nvidia-smi GPU UUID form, e.g.
// "GPU-4a1b2c3d-1122-3344-5566-778899aabbcc". MIG device UUIDs ("MIG-…") are
// deliberately rejected: MIG is disabled on the sandbox pool.
var canonical = regexp.MustCompile(`^GPU-[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// IsCanonical reports whether s is a full NVIDIA GPU UUID. The controller
// requires one before a GPU sandbox may become public READY, so a device index,
// a device-node name ("nvidia0"), or an empty string never satisfies the gate.
func IsCanonical(s string) bool { return canonical.MatchString(s) }

// Fake is a canonical-form UUID for tests that need the READY gate to pass
// without a real inspector.
const Fake = "GPU-00000000-0000-0000-0000-000000000000"
