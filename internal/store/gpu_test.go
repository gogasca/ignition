package store_test

import (
	"testing"

	"ignition.dev/ignition/internal/store"
)

func TestValidGPUType(t *testing.T) {
	if !store.ValidGPUType(store.GPUTypeNVIDIAL4) {
		t.Fatal("NVIDIA_L4 must be valid")
	}
	for _, bad := range []string{"", "nvidia-l4", "L4", "NVIDIA_H100", "UNSPECIFIED"} {
		if store.ValidGPUType(bad) {
			t.Fatalf("%q must not be a GpuType", bad)
		}
	}
}
