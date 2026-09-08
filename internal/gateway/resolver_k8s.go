package gateway

import (
	"context"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"ignition.dev/ignition/internal/k8s"
)

// K8sResolver finds a sandbox Pod by the ignition.io/sandbox-id label in the
// sandbox namespace. It needs only namespaced Pod list.
type K8sResolver struct {
	client kubernetes.Interface
	ns     string
}

func NewK8sResolver(cfg *rest.Config, namespace string) (*K8sResolver, error) {
	c, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return NewK8sResolverWithClient(c, namespace), nil
}

func NewK8sResolverWithClient(c kubernetes.Interface, namespace string) *K8sResolver {
	if namespace == "" {
		namespace = k8s.Namespace
	}
	return &K8sResolver{client: c, ns: namespace}
}

func (k *K8sResolver) Resolve(ctx context.Context, projectID, sandboxID string) (Endpoint, error) {
	sel := fmt.Sprintf("%s=%s,%s=%s",
		k8s.LabelSandboxID, sandboxID, k8s.LabelWorkload, k8s.WorkloadSandbox)
	list, err := k.client.CoreV1().Pods(k.ns).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return Endpoint{}, err
	}
	for i := range list.Items {
		pod := &list.Items[i]
		if projectID != "" && pod.Labels[k8s.LabelProjectID] != projectID {
			continue
		}
		if pod.DeletionTimestamp != nil || pod.Status.PodIP == "" {
			continue
		}
		return Endpoint{
			PodIP:      pod.Status.PodIP,
			Ready:      sandboxReady(pod),
			Generation: podGeneration(pod),
		}, nil
	}
	return Endpoint{}, fmt.Errorf("no running Pod for sandbox %s", sandboxID)
}

// sandboxReady mirrors ignition-controller's readiness rule (controller.podReady):
// for a CPU sandbox kubelet PodReady (backed by sandbox-init /readyz) is the whole
// signal; a GPU sandbox additionally requires ignition-gpu-agent's attestation —
// init-healthy=true plus a canonical GPU UUID — because the sandbox Pod holds no
// Kubernetes credential and only the agent can write those annotations. An
// unknown/absent accelerator class is treated as GPU (fail closed), matching
// k8s.IsGPUProfile.
func sandboxReady(p *corev1.Pod) bool {
	if !podReady(p) {
		return false
	}
	if !k8s.IsGPUProfile(p.Annotations[k8s.AnnotGPUType]) {
		return true
	}
	return p.Annotations[k8s.AnnotInitHealthy] == "true" &&
		k8s.IsCanonicalGPUUUID(p.Annotations[k8s.AnnotGPUUUID])
}

// podGeneration reads the sandbox generation ignition-controller stamps on the
// Pod. 0 ("unknown", e.g. a Pod predating the annotation) disables the gateway's
// stale-generation check rather than failing an otherwise-valid attach.
func podGeneration(p *corev1.Pod) int64 {
	n, err := strconv.ParseInt(p.Annotations[k8s.AnnotGeneration], 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
