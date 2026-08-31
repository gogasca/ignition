package store

import "fmt"

// Platform caps for a resolved RuntimeSpec. Enforced both when validating a
// CreateSandbox request and when validating the system default runtime at
// process startup.
const (
	MaxCPUMilli       = 8000
	MaxMemoryMiB      = 32768
	MaxStartupSeconds = 600
	MaxRuntimeSeconds = 86400
	MaxIdleSeconds    = 3600
	MaxGraceSeconds   = 120
)

// RuntimeSpec is a sandbox's run configuration. On CreateSandbox every field is
// optional; each unset field is filled from the system default runtime. The
// resolved spec is snapshotted onto the sandbox.
type RuntimeSpec struct {
	Resources ResourceSpec  `json:"resources"`
	Placement PlacementSpec `json:"placement"`
	Timeouts  TimeoutSpec   `json:"timeouts"`
	Network   NetworkSpec   `json:"network"`
}

// BuiltinDefaultRuntime is the fallback system runtime: a CPU-only sandbox with
// conservative timeouts and no internet access. Operators override it with
// IGNITION_DEFAULT_RUNTIME.
func BuiltinDefaultRuntime() RuntimeSpec {
	return RuntimeSpec{
		Resources: ResourceSpec{
			CPUMilli:    1000,
			MemoryMiB:   2048,
			Accelerator: AcceleratorSpec{Type: AcceleratorNone},
		},
		Placement: PlacementSpec{ComputeEnvironment: ComputeEnvironmentStandard},
		Timeouts: TimeoutSpec{
			StartupSeconds:          120,
			MaximumRuntimeSeconds:   3600,
			IdleSeconds:             600,
			TerminationGraceSeconds: 20,
		},
		Network: NetworkSpec{InternetAccess: InternetAccessDisabled},
	}
}

// MergeRuntime returns base with every field that req sets overriding it.
// A field is "set" when it is non-zero / non-empty.
func MergeRuntime(base, req RuntimeSpec) RuntimeSpec {
	out := base
	if req.Resources.CPUMilli > 0 {
		out.Resources.CPUMilli = req.Resources.CPUMilli
	}
	if req.Resources.MemoryMiB > 0 {
		out.Resources.MemoryMiB = req.Resources.MemoryMiB
	}
	if req.Resources.Accelerator.Type != "" {
		out.Resources.Accelerator = req.Resources.Accelerator
	}
	if req.Placement.Region != "" {
		out.Placement.Region = req.Placement.Region
	}
	if req.Placement.ComputeEnvironment != "" {
		out.Placement.ComputeEnvironment = req.Placement.ComputeEnvironment
	}
	if req.Timeouts.StartupSeconds > 0 {
		out.Timeouts.StartupSeconds = req.Timeouts.StartupSeconds
	}
	if req.Timeouts.MaximumRuntimeSeconds > 0 {
		out.Timeouts.MaximumRuntimeSeconds = req.Timeouts.MaximumRuntimeSeconds
	}
	if req.Timeouts.IdleSeconds > 0 {
		out.Timeouts.IdleSeconds = req.Timeouts.IdleSeconds
	}
	if req.Timeouts.TerminationGraceSeconds > 0 {
		out.Timeouts.TerminationGraceSeconds = req.Timeouts.TerminationGraceSeconds
	}
	if req.Network.InternetAccess != "" {
		out.Network.InternetAccess = req.Network.InternetAccess
	}
	return out
}

// ValidateRuntimeSpec checks a fully-resolved spec against platform caps and
// enum sets. It does not check project policy (region, accelerator allowlist);
// callers layer those on top.
func ValidateRuntimeSpec(s RuntimeSpec) error {
	if s.Resources.CPUMilli < 1 || s.Resources.CPUMilli > MaxCPUMilli {
		return fmt.Errorf("resources.cpuMilli must be between 1 and %d", MaxCPUMilli)
	}
	if s.Resources.MemoryMiB < 1 || s.Resources.MemoryMiB > MaxMemoryMiB {
		return fmt.Errorf("resources.memoryMiB must be between 1 and %d", MaxMemoryMiB)
	}
	if !ValidAccelerator(s.Resources.Accelerator.Type) {
		return fmt.Errorf("accelerator.type %q is not a supported AcceleratorType", s.Resources.Accelerator.Type)
	}
	if want, fixed := RequiredAcceleratorCount(s.Resources.Accelerator.Type); fixed && s.Resources.Accelerator.Count != want {
		return fmt.Errorf("accelerator.count must be %d for accelerator.type %q", want, s.Resources.Accelerator.Type)
	}
	if s.Timeouts.StartupSeconds < 1 || s.Timeouts.StartupSeconds > MaxStartupSeconds {
		return fmt.Errorf("timeouts.startupSeconds must be between 1 and %d", MaxStartupSeconds)
	}
	if s.Timeouts.MaximumRuntimeSeconds < 1 || s.Timeouts.MaximumRuntimeSeconds > MaxRuntimeSeconds {
		return fmt.Errorf("timeouts.maximumRuntimeSeconds must be between 1 and %d", MaxRuntimeSeconds)
	}
	if s.Timeouts.IdleSeconds < 1 || s.Timeouts.IdleSeconds > MaxIdleSeconds {
		return fmt.Errorf("timeouts.idleSeconds must be between 1 and %d", MaxIdleSeconds)
	}
	if s.Timeouts.TerminationGraceSeconds < 1 || s.Timeouts.TerminationGraceSeconds > MaxGraceSeconds {
		return fmt.Errorf("timeouts.terminationGraceSeconds must be between 1 and %d", MaxGraceSeconds)
	}
	switch s.Network.InternetAccess {
	case InternetAccessDisabled, InternetAccessEnabled:
	default:
		return fmt.Errorf("invalid network.internetAccess")
	}
	switch s.Placement.ComputeEnvironment {
	case ComputeEnvironmentStandard, ComputeEnvironmentBareMetal:
	default:
		return fmt.Errorf("invalid placement.computeEnvironment")
	}
	return nil
}
