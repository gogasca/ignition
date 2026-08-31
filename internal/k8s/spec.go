package k8s

import (
	"encoding/json"

	"ignition.dev/ignition/internal/store"
)

func boolPtr(v bool) *bool    { return &v }
func int64Ptr(v int64) *int64 { return &v }

// SandboxPod is the server-owned GKE Sandbox profile. Client fields never
// become hooks, hostPath, capabilities, or scheduling directives.
func SandboxPod(sb store.Sandbox, imageRef string) *Pod {
	grace := int64(sb.Timeouts.TerminationGraceSeconds)
	if grace <= 0 {
		grace = 20
	}
	if grace > 120 {
		grace = 120
	}
	deadline := int64(sb.Timeouts.MaximumRuntimeSeconds)
	if deadline <= 0 {
		deadline = 3600
	}
	if deadline > 86400 {
		deadline = 86400
	}
	cpu := sb.Resources.CPUMilli
	if cpu < 1 {
		cpu = 1
	}
	if cpu > 8000 {
		cpu = 8000
	}
	mem := sb.Resources.MemoryMiB
	if mem < 1 {
		mem = 1
	}
	if mem > 32768 {
		mem = 32768
	}
	cmdJSON, _ := json.Marshal(sb.Command)
	gpuType := sb.Resources.Accelerator.Type
	if gpuType == "" {
		gpuType = store.AcceleratorNVIDIAL4
	}
	env := map[string]string{
		"IGNITION_SANDBOX_ID": sb.ID,
		"IGNITION_PROJECT_ID": sb.ProjectID,
	}
	return &Pod{
		Name:      PodName(sb.ID),
		Namespace: Namespace,
		Labels: map[string]string{
			LabelWorkload:  WorkloadSandbox,
			LabelSandboxID: sb.ID,
			LabelProjectID: sb.ProjectID,
		},
		Annotations: map[string]string{
			AnnotImageID: sb.ImageID,
			AnnotCommand: string(cmdJSON),
			AnnotGPUType: gpuType,
		},
		Phase: "Pending",
		Spec: PodSpec{
			RuntimeClassName:             RuntimeClass,
			PriorityClassName:            PrioritySandbox,
			AutomountServiceAccountToken: boolPtr(false),
			EnableServiceLinks:           boolPtr(false),
			RestartPolicy:                "Never",
			ActiveDeadlineSeconds:        int64Ptr(deadline),
			TerminationGraceSeconds:      int64Ptr(grace),
			NodeSelector:                 map[string]string{GPUNodePoolLabel: NodePoolForGPUType(gpuType)},
			Tolerations: []Toleration{{
				Key: "ignition.io/gpu-sandbox", Operator: "Equal", Value: "true", Effect: "NoSchedule",
			}},
			AntiAffinityHostname:  true,
			RunAsNonRoot:          true,
			SeccompRuntimeDefault: true,
			Containers: []Container{{
				Name:            "sandbox",
				Image:           imageRef,
				Command:         []string{"/ignition/init"},
				Env:             env,
				WorkingDir:      sb.WorkingDir,
				CPUMilli:        cpu,
				MemoryMiB:       mem,
				GPU:             "1",
				AllowPrivEsc:    false,
				DropAllCaps:     true,
				ReadOnlyRootFS:  true,
				VolumeMountPath: "/scratch",
				Port:            8081,
				LivenessPath:    "/healthz",
				ReadinessPath:   "/readyz",
			}},
			Volumes: []Volume{{Name: "scratch", EmptyDir: true, SizeLimit: "20Gi"}},
		},
	}
}

// BalloonPod holds one GPU so Cluster Autoscaler keeps a warm node.
func BalloonPod(name string) *Pod {
	return &Pod{
		Name:      name,
		Namespace: Namespace,
		Labels:    map[string]string{LabelWorkload: WorkloadBalloon},
		Phase:     "Pending",
		Spec: PodSpec{
			RuntimeClassName:             RuntimeClass,
			PriorityClassName:            PriorityBalloon,
			AutomountServiceAccountToken: boolPtr(false),
			RestartPolicy:                "Always",
			NodeSelector:                 map[string]string{GPUNodePoolLabel: GPUNodePoolValue},
			Tolerations: []Toleration{{
				Key: "ignition.io/gpu-sandbox", Operator: "Equal", Value: "true", Effect: "NoSchedule",
			}},
			Containers: []Container{{
				Name:        "pause",
				Image:       "registry.k8s.io/pause:3.9",
				Command:     []string{"/pause"},
				CPUMilli:    100,
				MemoryMiB:   128,
				GPU:         "1",
				DropAllCaps: true,
			}},
		},
	}
}

// ApplySecretEnv injects resolved Secret Manager values as container env.
func ApplySecretEnv(p *Pod, secrets map[string]string) {
	if p == nil || len(secrets) == 0 || len(p.Spec.Containers) == 0 {
		return
	}
	if p.Spec.Containers[0].Env == nil {
		p.Spec.Containers[0].Env = map[string]string{}
	}
	for k, v := range secrets {
		p.Spec.Containers[0].Env[k] = v
	}
}

// NodePoolForGPUType maps a public GpuType to the GKE node-pool label value.
// Unknown types return empty so the controller can fail closed.
func NodePoolForGPUType(t string) string {
	switch t {
	case "", store.AcceleratorNVIDIAL4:
		return GPUNodePoolValue
	default:
		return ""
	}
}
