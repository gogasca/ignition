package store

// GPU types are the public JSON form of proto GpuType (prefix stripped).
const GPUTypeNVIDIAL4 = "NVIDIA_L4"

var gpuTypes = map[string]struct{}{
	GPUTypeNVIDIAL4: {},
}

// ValidGPUType reports whether t is a defined GpuType enum value.
func ValidGPUType(t string) bool {
	_, ok := gpuTypes[t]
	return ok
}
