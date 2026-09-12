package k8s

import (
	"strconv"

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
	// sb.Environment is validated at admission (internal/api's
	// checkEnvironment) to exclude store.ReservedEnvPrefix, but this is
	// defense in depth: the prefix check here — not just membership in the
	// three keys already in env above — always wins regardless of what a
	// Sandbox row happens to carry, including a reserved key added here later
	// that admission validation forgot to also start rejecting.
	for k, v := range sb.Environment {
		if store.IsReservedEnvName(k) {
			continue
		}
		env[k] = v
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
		RunAsUser:                    int64Ptr(SandboxUID),
		RunAsGroup:                   int64Ptr(SandboxGID),
		SeccompRuntimeDefault:        true,
		Containers:                   []Container{sandboxContainer(sb, imageRef, profile, cpu, mem, env)},
		Volumes: []Volume{
			{Name: "scratch", EmptyDir: true, SizeLimit: "20Gi"},
			// Projects the controller's desired-process annotation as a
			// read-only file; sandbox-init has no Kubernetes credentials.
			{Name: "ignition-pod", DownwardAPI: []DownwardAPIItem{{
				Path:      "process-desired",
				FieldPath: "metadata.annotations['" + AnnotProcDesired + "']",
			}}},
		},
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
			AnnotGPUType:          accel,
			AnnotGeneration:       strconv.FormatInt(sb.Generation, 10),
			AnnotNativeEntrypoint: boolStr(sb.NativeEntrypoint),
		},
		Phase: "Pending",
		Spec:  spec,
	}
}

// sandboxContainer builds the sandbox's single container. Managed mode (the
// default) runs Ignition's sandbox-init supervisor as PID 1 and gates public
// readiness on its /readyz probe; the sandbox's command/args become a
// supervised main process (a processes row) rather than the container command.
// NativeEntrypoint mode runs the admitted image's own OCI Entrypoint/Cmd —
// required for any image that does not embed sandbox-init — with Kubernetes
// command/args semantics: sb.Command overrides ENTRYPOINT, sb.Args overrides
// CMD, either unset falls back to the image. It drops the HTTP probes (a
// generic image serves neither /healthz nor /readyz), so kubelet reports Ready
// as soon as the container is Running and exec/idle-tracking are unavailable.
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
		c.Command = sb.Command
		c.Args = sb.Args
		return c
	}
	c.Command = []string{"/ignition/init"}
	c.Port = 8081
	c.LivenessPath = "/healthz"
	c.ReadinessPath = "/readyz"
	// The controller's desired-process annotation, projected read-only for the
	// supervisor (it holds no Kubernetes credentials).
	c.Mounts = []Mount{{Name: "ignition-pod", MountPath: "/etc/ignition/pod", ReadOnly: true}}
	return c
}

// BalloonPod holds a node of the given profile's class so Cluster Autoscaler
// keeps it warm. AnnotGPUType records the class (any accelerator type, not
// just GPU — the annotation predates CPU balloon support) so the controller
// can bucket balloons, busy sandboxes, and queued creates per class.
// DefaultBalloonImage is the upstream Kubernetes pause image: a do-nothing
// binary used only to reserve a warm node's resources, never actual sandbox
// workload. It's the right default for a cluster with unrestricted node
// egress, but Ignition's own Terraform (deploy/terraform/main.tf) ships a
// default-deny node egress policy that allows Google APIs/Artifact Registry
// via Private Google Access and nothing else — registry.k8s.io moved off
// Google-owned infrastructure and isn't reachable from a locked-down sandbox
// node pool. Deployments with that policy must set image to a mirror pushed
// into their own Artifact Registry (IGNITION_BALLOON_IMAGE).
const DefaultBalloonImage = "registry.k8s.io/pause:3.9"

func BalloonPod(name string, profile Profile, image string) *Pod {
	if image == "" {
		image = DefaultBalloonImage
	}
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
				Image:          image,
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
