package k8s

import "strings"

// PodName maps a product sandbox id (sbx_…) onto a DNS-1123 Pod name (sbx-…).
func PodName(sandboxID string) string {
	return strings.ToLower(strings.ReplaceAll(sandboxID, "_", "-"))
}

// SandboxIDFromPodName reverses PodName for ids with a single underscore.
func SandboxIDFromPodName(name string) string {
	return strings.Replace(name, "-", "_", 1)
}
