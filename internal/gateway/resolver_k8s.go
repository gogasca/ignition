package gateway

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"ignition.dev/ignition/internal/k8s"
)

// K8sResolver finds a sandbox Pod by label across all namespaces (sandboxes may
// live in per-project namespaces). It needs only Pod get/list.
type K8sResolver struct {
	client kubernetes.Interface
}

func NewK8sResolver(cfg *rest.Config) (*K8sResolver, error) {
	c, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &K8sResolver{client: c}, nil
}

func NewK8sResolverWithClient(c kubernetes.Interface) *K8sResolver {
	return &K8sResolver{client: c}
}

func (k *K8sResolver) Resolve(ctx context.Context, projectID, sandboxID string) (Endpoint, error) {
	sel := fmt.Sprintf("%s=%s,%s=%s",
		k8s.LabelSandboxID, sandboxID, k8s.LabelWorkload, k8s.WorkloadSandbox)
	list, err := k.client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{LabelSelector: sel})
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
			PodIP: pod.Status.PodIP,
			Ready: podReady(pod) && pod.Annotations[k8s.AnnotInitHealthy] == "true",
		}, nil
	}
	return Endpoint{}, fmt.Errorf("no running Pod for sandbox %s", sandboxID)
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
