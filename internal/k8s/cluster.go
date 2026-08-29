package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func NewClusterWithClient(client kubernetes.Interface, namespace string) *Cluster {
	if namespace == "" {
		namespace = Namespace
	}
	return &Cluster{client: client, ns: namespace}
}

const apiTimeout = 15 * time.Second

// Cluster talks to a real Kubernetes API (GKE in production).
type Cluster struct {
	client kubernetes.Interface
	ns     string
}

// RESTConfig loads in-cluster config, or a kubeconfig path for operators.
func RESTConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig == "" {
		kubeconfig = os.Getenv("KUBECONFIG")
	}
	var (
		cfg *rest.Config
		err error
	)
	if kubeconfig != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		cfg, err = rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("kubernetes config (set KUBECONFIG or run in-cluster): %w", err)
		}
	}
	if err != nil {
		return nil, err
	}
	cfg.UserAgent = "ignition-controller"
	cfg.QPS = 20
	cfg.Burst = 40
	return cfg, nil
}

func NewCluster(cfg *rest.Config, namespace string) (*Cluster, error) {
	if namespace == "" {
		namespace = Namespace
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "ignition-controller"
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Cluster{client: client, ns: namespace}, nil
}

func (c *Cluster) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), apiTimeout)
}

func (c *Cluster) Get(name string) (*Pod, error) {
	ctx, cancel := c.ctx()
	defer cancel()
	p, err := c.client.CoreV1().Pods(c.ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return fromCorev1(p), nil
}

func (c *Cluster) List() ([]Pod, error) {
	ctx, cancel := c.ctx()
	defer cancel()
	list, err := c.client.CoreV1().Pods(c.ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]Pod, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, *fromCorev1(&list.Items[i]))
	}
	return out, nil
}

func (c *Cluster) Create(p *Pod) error {
	core, err := toCorev1(p)
	if err != nil {
		return err
	}
	if core.Namespace == "" {
		core.Namespace = c.ns
	}
	ctx, cancel := c.ctx()
	defer cancel()
	_, err = c.client.CoreV1().Pods(c.ns).Create(ctx, core, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return ErrAlreadyExists
	}
	return err
}

func (c *Cluster) Delete(name string) error {
	ctx, cancel := c.ctx()
	defer cancel()
	err := c.client.CoreV1().Pods(c.ns).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// CordonAndDelete marks the node unschedulable after confirming it is in the
// GPU sandbox pool. It does not delete the Node object; GKE owns the MIG.
func (c *Cluster) CordonAndDelete(nodeName string) error {
	ctx, cancel := c.ctx()
	defer cancel()
	node, err := c.client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if node.Labels[GPUNodePoolLabel] != GPUNodePoolValue {
		return ErrCordonRefused
	}
	patch := []byte(`{"spec":{"unschedulable":true}}`)
	_, err = c.client.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, patch, metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return ErrNotFound
	}
	return err
}

func (c *Cluster) PatchAnnotations(name string, annotations map[string]string) error {
	if len(annotations) == 0 {
		return nil
	}
	body, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"annotations": annotations},
	})
	if err != nil {
		return err
	}
	ctx, cancel := c.ctx()
	defer cancel()
	_, err = c.client.CoreV1().Pods(c.ns).Patch(ctx, name, types.MergePatchType, body, metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return ErrNotFound
	}
	return err
}

func (c *Cluster) SetScaleDownDisabled(nodeName string, disabled bool) error {
	ctx, cancel := c.ctx()
	defer cancel()
	node, err := c.client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if node.Labels[GPUNodePoolLabel] != GPUNodePoolValue {
		return ErrCordonRefused
	}
	var val any = "true"
	if !disabled {
		val = nil
	}
	body, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]any{AnnotScaleDownDisabled: val},
		},
	})
	if err != nil {
		return err
	}
	_, err = c.client.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, body, metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return ErrNotFound
	}
	return err
}

func (c *Cluster) ListGPUPool() ([]string, error) {
	ctx, cancel := c.ctx()
	defer cancel()
	sel := GPUNodePoolLabel + "=" + GPUNodePoolValue
	list, err := c.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, list.Items[i].Name)
	}
	return out, nil
}

func (c *Cluster) ApplyNetworkPolicy(p *NetworkPolicy) error {
	if p == nil {
		return nil
	}
	core := toNetworkPolicy(p)
	ctx, cancel := c.ctx()
	defer cancel()
	np := c.client.NetworkingV1().NetworkPolicies(c.ns)
	existing, err := np.Get(ctx, core.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = np.Create(ctx, core, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	core.ResourceVersion = existing.ResourceVersion
	_, err = np.Update(ctx, core, metav1.UpdateOptions{})
	return err
}

func (c *Cluster) DeleteNetworkPolicy(name string) error {
	ctx, cancel := c.ctx()
	defer cancel()
	err := c.client.NetworkingV1().NetworkPolicies(c.ns).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
