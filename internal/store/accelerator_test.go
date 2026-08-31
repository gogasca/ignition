package store_test

import (
	"testing"

	"ignition.dev/ignition/internal/store"
)

func TestValidAccelerator(t *testing.T) {
	for _, ok := range []string{store.AcceleratorNVIDIAL4, store.AcceleratorNone} {
		if !store.ValidAccelerator(ok) {
			t.Fatalf("%q must be valid", ok)
		}
	}
	for _, bad := range []string{"", "nvidia-l4", "L4", "NVIDIA_H100", "UNSPECIFIED", "TPU_V5E"} {
		if store.ValidAccelerator(bad) {
			t.Fatalf("%q must not be an AcceleratorType", bad)
		}
	}
}

func TestAcceleratorIsGPU(t *testing.T) {
	if !store.AcceleratorIsGPU(store.AcceleratorNVIDIAL4) {
		t.Fatal("NVIDIA_L4 is a GPU")
	}
	if store.AcceleratorIsGPU(store.AcceleratorNone) {
		t.Fatal("NONE is not a GPU")
	}
}

func TestRequiredAcceleratorCount(t *testing.T) {
	cases := map[string]struct {
		want  int
		fixed bool
	}{
		store.AcceleratorNone:     {0, true},
		store.AcceleratorNVIDIAL4: {1, true},
		"TPU_V5E":                 {0, false},
	}
	for typ, exp := range cases {
		got, fixed := store.RequiredAcceleratorCount(typ)
		if got != exp.want || fixed != exp.fixed {
			t.Fatalf("%s: got (%d,%v) want (%d,%v)", typ, got, fixed, exp.want, exp.fixed)
		}
	}
}
