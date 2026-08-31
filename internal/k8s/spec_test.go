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
		ImageID:   "img_seed",
		Command:   []string{"python", "-m", "server"},
		Resources: store.ResourceSpec{CPUMilli: 4000, MemoryMiB: 16384, GPU: store.GPUSpec{Count: 1}},
		Timeouts:  store.TimeoutSpec{MaximumRuntimeSeconds: 3600, TerminationGraceSeconds: 20},
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
	if p.Annotations[k8s.AnnotCommand] == "" || !strings.Contains(p.Annotations[k8s.AnnotCommand], "python") {
		t.Fatal("tenant command must be annotation, not container command")
	}
	if p.Spec.Containers[0].Env["IGNITION_SANDBOX_ID"] != sb.ID {
		t.Fatal("sandbox id env")
	}
	if p.Annotations[k8s.AnnotGPUType] != store.GPUTypeNVIDIAL4 {
		t.Fatalf("gpu type annotation = %q", p.Annotations[k8s.AnnotGPUType])
	}
	if spec.NodeSelector[k8s.GPUNodePoolLabel] != k8s.GPUNodePoolValue {
		t.Fatalf("node pool = %v", spec.NodeSelector)
	}
}

func TestSandboxNetworkPolicyAllowList(t *testing.T) {
	sb := store.Sandbox{
		ID: "sbx_abc123def4567890ab",
		Network: store.NetworkSpec{Egress: store.EgressSpec{
			Mode: "ALLOW_LIST", AllowedCIDRs: []string{"1.2.3.0/24"},
		}},
	}
	np := k8s.SandboxNetworkPolicy(sb)
	if np.Name != "np-sbx-abc123def4567890ab" {
		t.Fatalf("name = %q", np.Name)
	}
	if !np.AllowDNS || np.EgressCIDRs[0] != "1.2.3.0/24" {
		t.Fatalf("%+v", np)
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
