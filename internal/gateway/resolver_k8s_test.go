package gateway_test

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"ignition.dev/ignition/internal/gateway"
	"ignition.dev/ignition/internal/gpuid"
	"ignition.dev/ignition/internal/k8s"
)

func sandboxPod(name, project, accel string, annots map[string]string) *corev1.Pod {
	a := map[string]string{k8s.AnnotGPUType: accel}
	for k, v := range annots {
		a[k] = v
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: k8s.Namespace,
			Labels: map[string]string{
				k8s.LabelWorkload:  k8s.WorkloadSandbox,
				k8s.LabelSandboxID: "sbx_1",
				k8s.LabelProjectID: project,
			},
			Annotations: a,
		},
		Status: corev1.PodStatus{
			PodIP: "10.1.2.3",
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

func resolve(t *testing.T, pods ...*corev1.Pod) gateway.Endpoint {
	t.Helper()
	c := fake.NewSimpleClientset()
	for _, p := range pods {
		if _, err := c.CoreV1().Pods(k8s.Namespace).Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed pod: %v", err)
		}
	}
	r := gateway.NewK8sResolverWithClient(c, k8s.Namespace)
	ep, err := r.Resolve(context.Background(), "prj_1", "sbx_1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return ep
}

func TestResolveCPUSandboxReadyOnPodReady(t *testing.T) {
	// A CPU sandbox never gets ignition-gpu-agent's init-healthy annotation;
	// kubelet PodReady alone must make it reachable.
	ep := resolve(t, sandboxPod("sbx-1", "prj_1", "NONE", nil))
	if !ep.Ready {
		t.Fatal("CPU sandbox with PodReady must resolve Ready")
	}
	if ep.PodIP != "10.1.2.3" {
		t.Fatalf("pod ip = %q", ep.PodIP)
	}
}

func TestResolveGPUSandboxNotReadyWithoutAttestation(t *testing.T) {
	ep := resolve(t, sandboxPod("sbx-1", "prj_1", "NVIDIA_L4", nil))
	if ep.Ready {
		t.Fatal("GPU sandbox without init-healthy/gpu-uuid must not be Ready")
	}
}

func TestResolveGPUSandboxReadyWithAttestation(t *testing.T) {
	ep := resolve(t, sandboxPod("sbx-1", "prj_1", "NVIDIA_L4", map[string]string{
		k8s.AnnotInitHealthy: "true",
		k8s.AnnotGPUUUID:     gpuid.Fake,
	}))
	if !ep.Ready {
		t.Fatal("attested GPU sandbox must resolve Ready")
	}
}

func TestResolveGPUSandboxNotReadyWithNonCanonicalUUID(t *testing.T) {
	ep := resolve(t, sandboxPod("sbx-1", "prj_1", "NVIDIA_L4", map[string]string{
		k8s.AnnotInitHealthy: "true",
		k8s.AnnotGPUUUID:     "nvidia0",
	}))
	if ep.Ready {
		t.Fatal("non-canonical GPU UUID must not satisfy readiness")
	}
}

func TestResolveReportsGeneration(t *testing.T) {
	ep := resolve(t, sandboxPod("sbx-1", "prj_1", "NONE", map[string]string{
		k8s.AnnotGeneration: "7",
	}))
	if ep.Generation != 7 {
		t.Fatalf("generation = %d, want 7", ep.Generation)
	}
}

func TestResolveGenerationZeroWhenAnnotationMissingOrBad(t *testing.T) {
	ep := resolve(t, sandboxPod("sbx-1", "prj_1", "NONE", map[string]string{
		k8s.AnnotGeneration: "not-a-number",
	}))
	if ep.Generation != 0 {
		t.Fatalf("generation = %d, want 0 (check disabled)", ep.Generation)
	}
}

func TestResolveSkipsOtherProjects(t *testing.T) {
	// Same sandbox-id label, different project: must not match.
	c := fake.NewSimpleClientset()
	p := sandboxPod("sbx-1", "prj_other", "NONE", nil)
	if _, err := c.CoreV1().Pods(k8s.Namespace).Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	r := gateway.NewK8sResolverWithClient(c, k8s.Namespace)
	if _, err := r.Resolve(context.Background(), "prj_1", "sbx_1"); err == nil {
		t.Fatal("expected no match for a pod owned by another project")
	}
}
