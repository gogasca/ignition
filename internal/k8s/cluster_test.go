package k8s_test

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"ignition.dev/ignition/internal/k8s"
	"ignition.dev/ignition/internal/store"
)

func TestToCorev1SandboxProfile(t *testing.T) {
	sb := store.Sandbox{
		ID:        "sbx_abc123def4567890ab",
		ProjectID: "prj_dev",
		ImageID:   "img_seed",
		Command:   []string{"python", "-m", "server"},
		Resources: store.ResourceSpec{CPUMilli: 4000, MemoryMiB: 16384, GPU: store.GPUSpec{Count: 1}},
		Timeouts:  store.TimeoutSpec{MaximumRuntimeSeconds: 3600, TerminationGraceSeconds: 20},
	}
	internal := k8s.SandboxPod(sb, "img@sha256:abc")
	core, err := k8s.ToCorev1(internal)
	if err != nil {
		t.Fatal(err)
	}
	if core.Spec.RuntimeClassName == nil || *core.Spec.RuntimeClassName != k8s.RuntimeClass {
		t.Fatalf("runtime = %v", core.Spec.RuntimeClassName)
	}
	if core.Spec.AutomountServiceAccountToken == nil || *core.Spec.AutomountServiceAccountToken {
		t.Fatal("must not automount SA token")
	}
	if core.Spec.Affinity == nil || core.Spec.Affinity.PodAntiAffinity == nil {
		t.Fatal("hostname anti-affinity required")
	}
	c := core.Spec.Containers[0]
	if len(c.Command) != 1 || c.Command[0] != "/ignition/init" {
		t.Fatalf("command = %v", c.Command)
	}
	if _, ok := c.Resources.Limits[corev1.ResourceName(k8s.GPUResource)]; !ok {
		t.Fatal("gpu limit missing")
	}
	if c.SecurityContext == nil || c.SecurityContext.ReadOnlyRootFilesystem == nil || !*c.SecurityContext.ReadOnlyRootFilesystem {
		t.Fatal("sandbox root filesystem must be read-only")
	}
	if c.LivenessProbe == nil || c.LivenessProbe.HTTPGet == nil || c.LivenessProbe.HTTPGet.Path != "/healthz" {
		t.Fatalf("liveness probe = %#v", c.LivenessProbe)
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.HTTPGet == nil || c.ReadinessProbe.HTTPGet.Path != "/readyz" {
		t.Fatalf("readiness probe = %#v", c.ReadinessProbe)
	}
	for _, v := range core.Spec.Volumes {
		if v.HostPath != nil {
			t.Fatal("hostPath forbidden")
		}
	}
}

func TestToCorev1RejectsHostPath(t *testing.T) {
	p := &k8s.Pod{
		Name: "x",
		Spec: k8s.PodSpec{Volumes: []k8s.Volume{{Name: "h", HostPath: "/etc"}}},
	}
	if _, err := k8s.ToCorev1(p); err == nil {
		t.Fatal("expected hostPath error")
	}
}

func TestFromCorev1Status(t *testing.T) {
	core := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-a", Namespace: k8s.Namespace},
		Spec:       corev1.PodSpec{NodeName: "gke-node-1"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	got := k8s.FromCorev1(core)
	if !got.Scheduled || !got.Running || !got.Ready {
		t.Fatalf("scheduled=%v running=%v ready=%v", got.Scheduled, got.Running, got.Ready)
	}
	if got.NodeName != "gke-node-1" {
		t.Fatalf("node = %q", got.NodeName)
	}
}

func TestFromCorev1ImagePull(t *testing.T) {
	core := &corev1.Pod{
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull"},
				},
			}},
		},
	}
	got := k8s.FromCorev1(core)
	if got.Reason != "ErrImagePull" {
		t.Fatalf("reason = %q", got.Reason)
	}
}

func TestClusterFakeClientset(t *testing.T) {
	fc := fake.NewSimpleClientset()
	c := k8s.NewClusterWithClient(fc, k8s.Namespace)
	p := k8s.BalloonPod("balloon-0")
	if err := c.Create(p); err != nil {
		t.Fatal(err)
	}
	got, err := c.Get("balloon-0")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "balloon-0" {
		t.Fatalf("name = %q", got.Name)
	}
	if err := c.Create(p); err != k8s.ErrAlreadyExists {
		t.Fatalf("err = %v", err)
	}
	if err := c.Delete("balloon-0"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get("balloon-0"); err != k8s.ErrNotFound {
		t.Fatalf("get after delete: %v", err)
	}
}

func TestCordonRequiresGPUPoolLabel(t *testing.T) {
	gpu := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   "gpu-1",
		Labels: map[string]string{k8s.GPUNodePoolLabel: k8s.GPUNodePoolValue},
	}}
	other := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "other"}}
	fc := fake.NewSimpleClientset(gpu, other)
	c := k8s.NewClusterWithClient(fc, k8s.Namespace)
	if err := c.CordonAndDelete("gpu-1"); err != nil {
		t.Fatal(err)
	}
	if err := c.CordonAndDelete("other"); err != k8s.ErrCordonRefused {
		t.Fatalf("err = %v", err)
	}
}

func TestSetScaleDownDisabled(t *testing.T) {
	gpu := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   "gpu-1",
		Labels: map[string]string{k8s.GPUNodePoolLabel: k8s.GPUNodePoolValue},
	}}
	fc := fake.NewSimpleClientset(gpu)
	c := k8s.NewClusterWithClient(fc, k8s.Namespace)
	if err := c.SetScaleDownDisabled("gpu-1", true); err != nil {
		t.Fatal(err)
	}
	got, err := fc.CoreV1().Nodes().Get(context.Background(), "gpu-1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Annotations[k8s.AnnotScaleDownDisabled] != "true" {
		t.Fatalf("annotations = %v", got.Annotations)
	}
	if err := c.SetScaleDownDisabled("gpu-1", false); err != nil {
		t.Fatal(err)
	}
}

func TestApplyNetworkPolicy(t *testing.T) {
	fc := fake.NewSimpleClientset()
	c := k8s.NewClusterWithClient(fc, k8s.Namespace)
	np := &k8s.NetworkPolicy{
		Name: "np-sbx-a", SandboxID: "sbx_a", AllowDNS: true,
		EgressCIDRs: []string{"10.0.0.0/8"},
	}
	if err := c.ApplyNetworkPolicy(np); err != nil {
		t.Fatal(err)
	}
	got, err := fc.NetworkingV1().NetworkPolicies(k8s.Namespace).Get(context.Background(), "np-sbx-a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Spec.PodSelector.MatchLabels[k8s.LabelSandboxID] != "sbx_a" {
		t.Fatalf("selector = %v", got.Spec.PodSelector)
	}
	if err := c.ApplyNetworkPolicy(np); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteNetworkPolicy("np-sbx-a"); err != nil {
		t.Fatal(err)
	}
}
