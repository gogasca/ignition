package k8s

import (
	"encoding/json"

	"ignition.dev/ignition/internal/store"
)

func boolPtr(v bool) *bool    { return &v }
func int64Ptr(v int64) *int64 { return &v }

func boolStr(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

// SandboxPod is the server-owned GKE Sandbox profile. Client fields never
// become hooks, hostPath, capabilities, or scheduling directives.
func SandboxPod(sb store.Sandbox, imageRef string) *Pod {
	networkAccess := NetworkAccessDisabled
	if sb.Network.InternetAccess == store.InternetAccessEnabled {
		networkAccess = NetworkAccessEnabled
	}
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
	accel := sb.Resources.Accelerator.Type
	if accel == "" {
		accel = store.AcceleratorNVIDIAL4
	}
	// The caller (reconcile) fails closed when no profile exists; default to
	// the L4 profile only so a mis-sequenced call still produces a valid Pod.
	profile, ok := ProfileForNetwork(accel, sb.Network.InternetAccess == store.InternetAccessEnabled)
	if !ok {
		profile = profiles[store.AcceleratorNVIDIAL4]
	}
	env := map[string]string{
		"IGNITION_SANDBOX_ID": sb.ID,
		"IGNITION_PROJECT_ID": sb.ProjectID,
		EnvAccelerator:        accel,
	}
	spec := PodSpec{
		RuntimeClassName:             RuntimeClass,
		PriorityClassName:            PrioritySandbox,
		AutomountServiceAccountToken: boolPtr(false),
		EnableServiceLinks:           boolPtr(false),
		RestartPolicy:                "Never",
		ActiveDeadlineSeconds:        int64Ptr(deadline),
		TerminationGraceSeconds:      int64Ptr(grace),
		NodeSelector:                 map[string]string{NodePoolLabel: profile.NodePoolValue},
		AntiAffinityHostname:         profile.AntiAffinity,
		RunAsNonRoot:                 true,
		SeccompRuntimeDefault:        true,
		Containers:                   []Container{sandboxContainer(sb, imageRef, profile, cpu, mem, env)},
		Volumes:                      []Volume{{Name: "scratch", EmptyDir: true, SizeLimit: "20Gi"}},
	}
	if profile.TaintKey != "" {
		spec.Tolerations = []Toleration{{
			Key: profile.TaintKey, Operator: "Equal", Value: "true", Effect: "NoSchedule",
		}}
	}
	return &Pod{
		Name:      PodName(sb.ID),
		Namespace: Namespace,
		Labels: map[string]string{
			LabelWorkload:      WorkloadSandbox,
			LabelSandboxID:     sb.ID,
			LabelProjectID:     sb.ProjectID,
			LabelNetworkAccess: networkAccess,
		},
		Annotations: map[string]string{
			AnnotImageID:          sb.ImageID,
			AnnotCommand:          string(cmdJSON),
			AnnotGPUType:          accel,
			AnnotNativeEntrypoint: boolStr(sb.NativeEntrypoint),
		},
		Phase: "Pending",
		Spec:  spec,
	}
}

// sandboxContainer builds the sandbox's single container. Managed mode (the
// default) runs Ignition's sandbox-init supervisor as PID 1 and gates public
// readiness on its /readyz probe. NativeEntrypoint mode runs the admitted
// image's own OCI Entrypoint/Cmd unchanged — required for any image that does
// not embed sandbox-init — and drops the HTTP probes, since a generic image
// serves neither /healthz nor /readyz: kubelet then reports the container
// Ready as soon as it is Running, and command/exec/idle-tracking are
// unavailable for the sandbox (no supervisor to relay them to).
func sandboxContainer(sb store.Sandbox, imageRef string, profile Profile, cpu, mem int, env map[string]string) Container {
	c := Container{
		Name:            "sandbox",
		Image:           imageRef,
		Env:             env,
		WorkingDir:      sb.WorkingDir,
		CPUMilli:        cpu,
		MemoryMiB:       mem,
		GPU:             profile.GPUQuantity,
		AllowPrivEsc:    false,
		DropAllCaps:     true,
		ReadOnlyRootFS:  true,
		VolumeMountPath: "/scratch",
	}
	if sb.NativeEntrypoint {
		return c
	}
	c.Command = []string{"/ignition/init"}
	c.Port = 8081
	c.LivenessPath = "/healthz"
	c.ReadinessPath = "/readyz"
	return c
}

// BalloonPod holds a node of the given profile's class so Cluster Autoscaler
// keeps it warm. AnnotGPUType records the class (any accelerator type, not
// just GPU — the annotation predates CPU balloon support) so the controller
// can bucket balloons, busy sandboxes, and queued creates per class.
func BalloonPod(name string, profile Profile) *Pod {
	p := &Pod{
		Name:        name,
		Namespace:   Namespace,
		Labels:      map[string]string{LabelWorkload: WorkloadBalloon},
		Annotations: map[string]string{AnnotGPUType: profile.Accelerator},
		Phase:       "Pending",
		Spec: PodSpec{
			RuntimeClassName:             RuntimeClass,
			PriorityClassName:            PriorityBalloon,
			AutomountServiceAccountToken: boolPtr(false),
			EnableServiceLinks:           boolPtr(false),
			RestartPolicy:                "Always",
			NodeSelector:                 map[string]string{NodePoolLabel: profile.NodePoolValue},
			RunAsNonRoot:                 true,
			SeccompRuntimeDefault:        true,
			Containers: []Container{{
				Name:           "pause",
				Image:          "registry.k8s.io/pause:3.9",
				Command:        []string{"/pause"},
				CPUMilli:       100,
				MemoryMiB:      128,
				GPU:            profile.GPUQuantity,
				AllowPrivEsc:   false,
				DropAllCaps:    true,
				ReadOnlyRootFS: true,
			}},
		},
	}
	if profile.TaintKey != "" {
		p.Spec.Tolerations = []Toleration{{
			Key: profile.TaintKey, Operator: "Equal", Value: "true", Effect: "NoSchedule",
		}}
	}
	return p
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
