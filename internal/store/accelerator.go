package store

// Accelerator types are the public JSON form of proto AcceleratorType
// (the ACCELERATOR_TYPE_ prefix is stripped). NONE is a CPU-only sandbox.
const (
	AcceleratorNone     = "NONE"
	AcceleratorNVIDIAL4 = "NVIDIA_L4"
)

var acceleratorTypes = map[string]struct{}{
	AcceleratorNone:     {},
	AcceleratorNVIDIAL4: {},
}

// ValidAccelerator reports whether t is a defined AcceleratorType enum value.
func ValidAccelerator(t string) bool {
	_, ok := acceleratorTypes[t]
	return ok
}

// AcceleratorIsGPU reports whether t names a GPU accelerator rather than a
// CPU-only sandbox.
func AcceleratorIsGPU(t string) bool {
	return t == AcceleratorNVIDIAL4
}

// RequiredAcceleratorCount returns the exact device count required for t. fixed
// is false when t is unknown, in which case the count is not constrained here.
func RequiredAcceleratorCount(t string) (count int, fixed bool) {
	switch t {
	case AcceleratorNone:
		return 0, true
	case AcceleratorNVIDIAL4:
		return 1, true
	default:
		return 0, false
	}
}
