package k8s_test

import (
	"strings"
	"testing"

	"ignition.dev/ignition/internal/k8s"
	"ignition.dev/ignition/internal/store"
)

func TestPodNameRoundTrip(t *testing.T) {
	id := "sbx_deadbeefcafebabe12"
	name := k8s.PodName(id)
	if strings.Contains(name, "_") {
		t.Fatalf("pod name %q contains underscore", name)
	}
	if got := k8s.SandboxIDFromPodName(name); got != id {
		t.Fatalf("round trip %q", got)
	}
}

func TestSandboxPodProfile(t *testing.T) {
	sb := store.Sandbox{
		ID:        "sbx_abc123def4567890ab",
		ProjectID: "prj_dev",
		SandboxSpec: store.SandboxSpec{
			ImageID:   "img_seed",
			Command:   []string{"python", "-m", "server"},
			Resources: store.ResourceSpec{CPUMilli: 4000, MemoryMiB: 16384, Accelerator: store.AcceleratorSpec{Count: 1}},
			Timeouts:  store.TimeoutSpec{MaximumRuntimeSeconds: 3600, TerminationGraceSeconds: 20},
		},
		Generation: 1,
	}
	p := k8s.SandboxPod(sb, "img@sha256:abc")
	if p.Name != "sbx-abc123def4567890ab" {
		t.Fatalf("name = %q", p.Name)
	}
	if p.Namespace != k8s.Namespace {
		t.Fatalf("ns = %q", p.Namespace)
	}
	spec := p.Spec
	if spec.RuntimeClassName != k8s.RuntimeClass {
		t.Fatalf("runtime = %q", spec.RuntimeClassName)
	}
	if spec.PriorityClassName != k8s.PrioritySandbox {
		t.Fatal("priority")
	}
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Fatal("must not automount SA token")
	}
	if spec.RestartPolicy != "Never" {
		t.Fatal("restart")
	}
	if !spec.AntiAffinityHostname {
		t.Fatal("hostname anti-affinity required")
	}
	if len(spec.Containers) != 1 {
		t.Fatal("one container")
	}
	c := spec.Containers[0]
	if len(c.Command) != 1 || c.Command[0] != "/ignition/init" {
		t.Fatalf("command must be init, got %v", c.Command)
	}
	if c.GPU != "1" {
		t.Fatalf("gpu = %q", c.GPU)
	}
	if c.AllowPrivEsc || !c.DropAllCaps {
		t.Fatal("privileges")
	}
	if !c.ReadOnlyRootFS {
		t.Fatal("sandbox root filesystem must be read-only")
	}
	if c.Port != 8081 || c.LivenessPath != "/healthz" || c.ReadinessPath != "/readyz" {
		t.Fatalf("supervisor probes = port %d liveness %q readiness %q", c.Port, c.LivenessPath, c.ReadinessPath)
	}
	for _, v := range spec.Volumes {
		if v.HostPath != "" {
			t.Fatal("hostPath forbidden")
		}
	}
	// The desired-process annotation is projected read-only for sandbox-init,
	// which has no Kubernetes credentials.
	var projected *k8s.Volume
	for i := range spec.Volumes {
		if len(spec.Volumes[i].DownwardAPI) > 0 {
			projected = &spec.Volumes[i]
		}
	}
	if projected == nil || projected.DownwardAPI[0].Path != "process-desired" ||
		!strings.Contains(projected.DownwardAPI[0].FieldPath, k8s.AnnotProcDesired) {
		t.Fatalf("missing process-desired downward-API projection: %+v", spec.Volumes)
	}
	var mounted bool
	for _, mnt := range c.Mounts {
		if mnt.Name == projected.Name && mnt.ReadOnly && mnt.MountPath == "/etc/ignition/pod" {
			mounted = true
		}
	}
	if !mounted {
		t.Fatalf("projected volume not mounted read-only: %+v", c.Mounts)
	}
	// Rendering to core/v1 must succeed with the new volume/mount.
	if _, err := k8s.ToCorev1(p); err != nil {
		t.Fatalf("ToCorev1: %v", err)
	}
	// Managed mode: the container command stays the supervisor; the sandbox's
	// command becomes a supervised main process (a processes row the API
	// creates), not a Pod annotation and not the container command.
	if got := c.Command; len(got) != 1 || got[0] != "/ignition/init" {
		t.Fatalf("managed container command = %v, want [/ignition/init]", got)
	}
	if p.Spec.Containers[0].Env["IGNITION_SANDBOX_ID"] != sb.ID {
		t.Fatal("sandbox id env")
	}
	if p.Annotations[k8s.AnnotGPUType] != store.AcceleratorNVIDIAL4 {
		t.Fatalf("gpu type annotation = %q", p.Annotations[k8s.AnnotGPUType])
	}
	if p.Annotations[k8s.AnnotGeneration] != "1" {
		t.Fatalf("generation annotation = %q", p.Annotations[k8s.AnnotGeneration])
	}
	if spec.NodeSelector[k8s.GPUNodePoolLabel] != k8s.GPUNodePoolValue {
		t.Fatalf("node pool = %v", spec.NodeSelector)
	}
}

func TestSandboxPodCPUProfile(t *testing.T) {
	sb := store.Sandbox{
		ID:        "sbx_cpu0000000000000000",
		ProjectID: "prj_dev",
		SandboxSpec: store.SandboxSpec{
			ImageID:   "img_seed",
			Resources: store.ResourceSpec{CPUMilli: 2000, MemoryMiB: 4096, Accelerator: store.AcceleratorSpec{Type: store.AcceleratorNone}},
			Timeouts:  store.TimeoutSpec{MaximumRuntimeSeconds: 3600, TerminationGraceSeconds: 20},
		},
	}
	p := k8s.SandboxPod(sb, "img@sha256:abc")
	spec := p.Spec
	if spec.RuntimeClassName != k8s.RuntimeClass {
		t.Fatalf("CPU sandbox still runs on gVisor; runtime = %q", spec.RuntimeClassName)
	}
	if spec.Containers[0].GPU != "" {
		t.Fatalf("CPU sandbox requested a GPU: %q", spec.Containers[0].GPU)
	}
	if spec.AntiAffinityHostname {
		t.Fatal("CPU sandbox should not require one-per-node anti-affinity")
	}
	if spec.NodeSelector[k8s.NodePoolLabel] != k8s.CPUNodePoolValue {
		t.Fatalf("node pool = %v", spec.NodeSelector)
	}
	if spec.Containers[0].Env[k8s.EnvAccelerator] != store.AcceleratorNone {
		t.Fatalf("IGNITION_ACCELERATOR = %q", spec.Containers[0].Env[k8s.EnvAccelerator])
	}
	if len(spec.Tolerations) != 1 || spec.Tolerations[0].Key != "ignition.io/sandbox" {
		t.Fatalf("tolerations = %v", spec.Tolerations)
	}
}

func TestSandboxPodNetworkProfile(t *testing.T) {
	sb := store.Sandbox{
		SandboxSpec: store.SandboxSpec{
			Network: store.NetworkSpec{InternetAccess: store.InternetAccessDisabled},
		},
	}
	if got := k8s.SandboxPod(sb, "img@sha256:abc").Labels[k8s.LabelNetworkAccess]; got != k8s.NetworkAccessDisabled {
		t.Fatalf("disabled network label = %q", got)
	}
	sb.Network.InternetAccess = store.InternetAccessEnabled
	if got := k8s.SandboxPod(sb, "img@sha256:abc").Labels[k8s.LabelNetworkAccess]; got != k8s.NetworkAccessEnabled {
		t.Fatalf("enabled network label = %q", got)
	}
	if got := k8s.SandboxPod(sb, "img@sha256:abc").Spec.NodeSelector[k8s.NodePoolLabel]; got != k8s.GPUInternetNodePoolValue {
		t.Fatalf("enabled GPU node pool = %q", got)
	}
	sb.Resources.Accelerator.Type = store.AcceleratorNone
	if got := k8s.SandboxPod(sb, "img@sha256:abc").Spec.NodeSelector[k8s.NodePoolLabel]; got != k8s.CPUInternetNodePoolValue {
		t.Fatalf("enabled CPU node pool = %q", got)
	}
}

func TestSandboxPodNativeEntrypoint(t *testing.T) {
	sb := store.Sandbox{
		ID:        "sbx_native0000000000",
		ProjectID: "prj_dev",
		SandboxSpec: store.SandboxSpec{
			ImageID:          "img_seed",
			NativeEntrypoint: true,
			Resources:        store.ResourceSpec{CPUMilli: 1000, MemoryMiB: 2048, Accelerator: store.AcceleratorSpec{Type: store.AcceleratorNone}},
			Timeouts:         store.TimeoutSpec{MaximumRuntimeSeconds: 3600, TerminationGraceSeconds: 20},
		},
	}
	c := k8s.SandboxPod(sb, "docker.io/library/nginx@sha256:abc").Spec.Containers[0]
	// No command/args set: the image's own ENTRYPOINT/CMD must stand (nil, not []).
	if c.Command != nil || c.Args != nil {
		t.Fatalf("native entrypoint with no overrides: command %v args %v, want nil/nil", c.Command, c.Args)
	}
	if c.Port != 0 || c.LivenessPath != "" || c.ReadinessPath != "" {
		t.Fatalf("native entrypoint must not probe a supervisor: port %d liveness %q readiness %q", c.Port, c.LivenessPath, c.ReadinessPath)
	}

	// With overrides: Kubernetes command/args semantics straight through.
	sb.Command = []string{"/bin/myserver"}
	sb.Args = []string{"--port", "9000"}
	oc := k8s.SandboxPod(sb, "docker.io/library/nginx@sha256:abc").Spec.Containers[0]
	if len(oc.Command) != 1 || oc.Command[0] != "/bin/myserver" {
		t.Fatalf("native command = %v", oc.Command)
	}
	if len(oc.Args) != 2 || oc.Args[0] != "--port" || oc.Args[1] != "9000" {
		t.Fatalf("native args = %v", oc.Args)
	}
	core, err := k8s.ToCorev1(k8s.SandboxPod(sb, "docker.io/library/nginx@sha256:abc"))
	if err != nil {
		t.Fatalf("ToCorev1: %v", err)
	}
	if got := core.Spec.Containers[0]; len(got.Command) != 1 || len(got.Args) != 2 {
		t.Fatalf("corev1 command %v args %v", got.Command, got.Args)
	}
	p := k8s.SandboxPod(sb, "docker.io/library/nginx@sha256:abc")
	if p.Annotations[k8s.AnnotNativeEntrypoint] != "true" {
		t.Fatalf("native entrypoint annotation = %q", p.Annotations[k8s.AnnotNativeEntrypoint])
	}
}

func TestSandboxPodManagedEntrypointAnnotation(t *testing.T) {
	sb := store.Sandbox{
		ID:        "sbx_managed000000000",
		ProjectID: "prj_dev",
		SandboxSpec: store.SandboxSpec{
			ImageID: "img_seed",
		},
	}
	p := k8s.SandboxPod(sb, "img@sha256:abc")
	if p.Annotations[k8s.AnnotNativeEntrypoint] != "false" {
		t.Fatalf("managed entrypoint annotation = %q", p.Annotations[k8s.AnnotNativeEntrypoint])
	}
}

func TestSandboxPodEnvironment(t *testing.T) {
	sb := store.Sandbox{
		ID:        "sbx_envtest0000000000",
		ProjectID: "prj_dev",
		SandboxSpec: store.SandboxSpec{
			ImageID:     "img_seed",
			Environment: map[string]string{"TASK_ID": "task_0001", "RUN_ID": "demo"},
		},
	}
	env := k8s.SandboxPod(sb, "img@sha256:abc").Spec.Containers[0].Env
	if env["TASK_ID"] != "task_0001" || env["RUN_ID"] != "demo" {
		t.Fatalf("container env = %v", env)
	}
	// The controller's own Pod env is always present alongside it.
	if env["IGNITION_SANDBOX_ID"] != sb.ID || env["IGNITION_PROJECT_ID"] != sb.ProjectID {
		t.Fatalf("reserved env missing: %v", env)
	}
}

// Defense in depth: even if a Sandbox row somehow carried a reserved key (the
// API layer is supposed to reject this at admission — internal/api's
// checkEnvironment), the Pod builder must never let it override the
// controller's own identity/accelerator env.
func TestSandboxPodEnvironmentCannotOverrideReservedKeys(t *testing.T) {
	sb := store.Sandbox{
		ID:        "sbx_envtest0000000001",
		ProjectID: "prj_dev",
		SandboxSpec: store.SandboxSpec{
			ImageID:   "img_seed",
			Resources: store.ResourceSpec{Accelerator: store.AcceleratorSpec{Type: store.AcceleratorNone}},
			Environment: map[string]string{
				"IGNITION_SANDBOX_ID": "evil",
				"IGNITION_PROJECT_ID": "evil",
				k8s.EnvAccelerator:    "evil",
				"SAFE_KEY":            "ok",
			},
		},
	}
	env := k8s.SandboxPod(sb, "img@sha256:abc").Spec.Containers[0].Env
	if env["IGNITION_SANDBOX_ID"] != sb.ID {
		t.Fatalf("IGNITION_SANDBOX_ID = %q, want %q (must not be overridable)", env["IGNITION_SANDBOX_ID"], sb.ID)
	}
	if env["IGNITION_PROJECT_ID"] != sb.ProjectID {
		t.Fatalf("IGNITION_PROJECT_ID = %q, want %q", env["IGNITION_PROJECT_ID"], sb.ProjectID)
	}
	if env[k8s.EnvAccelerator] == "evil" {
		t.Fatalf("%s was overridden: %q", k8s.EnvAccelerator, env[k8s.EnvAccelerator])
	}
	if env["SAFE_KEY"] != "ok" {
		t.Fatalf("non-reserved key was dropped: %v", env)
	}
}

func TestFakeCreateIdempotent(t *testing.T) {
	f := k8s.NewFake()
	p := &k8s.Pod{Name: "sbx-a", Namespace: k8s.Namespace}
	if err := f.Create(p); err != nil {
		t.Fatal(err)
	}
	if err := f.Create(p); err != k8s.ErrAlreadyExists {
		t.Fatalf("err = %v", err)
	}
	if f.Creates != 1 {
		t.Fatalf("creates = %d", f.Creates)
	}
	if err := f.Delete("sbx-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Get("sbx-a"); err != k8s.ErrNotFound {
		t.Fatalf("get after delete: %v", err)
	}
}
