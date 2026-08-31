package store_test

import (
	"testing"

	"ignition.dev/ignition/internal/store"
)

func TestBuiltinDefaultRuntimeIsValidCPU(t *testing.T) {
	rt := store.BuiltinDefaultRuntime()
	if err := store.ValidateRuntimeSpec(rt); err != nil {
		t.Fatalf("builtin default runtime invalid: %v", err)
	}
	if rt.Resources.Accelerator.Type != store.AcceleratorNone {
		t.Fatalf("builtin default is not CPU: %+v", rt.Resources.Accelerator)
	}
	if rt.Network.InternetAccess != store.InternetAccessDisabled {
		t.Fatalf("builtin default has internet: %+v", rt.Network)
	}
}

func TestMergeRuntimeFieldLevel(t *testing.T) {
	base := store.BuiltinDefaultRuntime()
	req := store.RuntimeSpec{
		Resources: store.ResourceSpec{
			Accelerator: store.AcceleratorSpec{Type: store.AcceleratorNVIDIAL4, Count: 1},
		},
		Timeouts: store.TimeoutSpec{IdleSeconds: 42},
	}
	got := store.MergeRuntime(base, req)

	if got.Resources.Accelerator.Type != store.AcceleratorNVIDIAL4 || got.Resources.Accelerator.Count != 1 {
		t.Fatalf("accelerator not overridden: %+v", got.Resources.Accelerator)
	}
	if got.Resources.CPUMilli != base.Resources.CPUMilli || got.Resources.MemoryMiB != base.Resources.MemoryMiB {
		t.Fatalf("cpu/mem should keep base values: %+v", got.Resources)
	}
	if got.Timeouts.IdleSeconds != 42 {
		t.Fatalf("idleSeconds not overridden: %d", got.Timeouts.IdleSeconds)
	}
	if got.Timeouts.StartupSeconds != base.Timeouts.StartupSeconds {
		t.Fatalf("startupSeconds should keep base: %d", got.Timeouts.StartupSeconds)
	}
	if got.Network.InternetAccess != base.Network.InternetAccess {
		t.Fatalf("network should keep base: %+v", got.Network)
	}
}

func TestValidateRuntimeSpecRejects(t *testing.T) {
	base := store.BuiltinDefaultRuntime()
	cases := map[string]func(*store.RuntimeSpec){
		"cpu over cap":    func(s *store.RuntimeSpec) { s.Resources.CPUMilli = store.MaxCPUMilli + 1 },
		"memory zero":     func(s *store.RuntimeSpec) { s.Resources.MemoryMiB = 0 },
		"bad accelerator": func(s *store.RuntimeSpec) { s.Resources.Accelerator.Type = "TPU_V5E" },
		"gpu count wrong": func(s *store.RuntimeSpec) {
			s.Resources.Accelerator = store.AcceleratorSpec{Type: store.AcceleratorNVIDIAL4, Count: 2}
		},
		"idle over cap":   func(s *store.RuntimeSpec) { s.Timeouts.IdleSeconds = store.MaxIdleSeconds + 1 },
		"bad internet":    func(s *store.RuntimeSpec) { s.Network.InternetAccess = "ALLOW_LIST" },
		"bad compute env": func(s *store.RuntimeSpec) { s.Placement.ComputeEnvironment = "AUTOMATIC" },
	}
	for name, mut := range cases {
		rt := base
		mut(&rt)
		if err := store.ValidateRuntimeSpec(rt); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}
