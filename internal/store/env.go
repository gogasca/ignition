package store

import "strings"

// ReservedEnvPrefix is the namespace internal/k8s.SandboxPod's own Pod env
// uses (IGNITION_SANDBOX_ID, IGNITION_PROJECT_ID, IGNITION_ACCELERATOR, and
// any future one). No client-supplied environment name — a plain
// CreateSandbox environment key or a secretRef's environmentName — may use
// it. Shared by internal/api (admission-time validation) and internal/k8s
// (defense in depth when building the container env) so the rule has exactly
// one implementation instead of drifting between the two call sites.
const ReservedEnvPrefix = "IGNITION_"

// IsReservedEnvName reports whether name is in the reserved namespace.
func IsReservedEnvName(name string) bool {
	return strings.HasPrefix(name, ReservedEnvPrefix)
}
