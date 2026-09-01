package k8s

import "ignition.dev/ignition/internal/gpuid"

// IsCanonicalGPUUUID reports whether s is a full NVIDIA GPU UUID. See
// internal/gpuid; re-exported here so controller code reads naturally.
func IsCanonicalGPUUUID(s string) bool { return gpuid.IsCanonical(s) }

// FakeGPUUUID is a canonical-form UUID for tests.
const FakeGPUUUID = gpuid.Fake
