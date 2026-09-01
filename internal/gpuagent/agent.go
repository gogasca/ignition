// Package gpuagent is the node-local trusted GPU authority. One instance runs
// per GPU sandbox node (a DaemonSet). Because the node holds exactly one GPU and
// at most one customer sandbox, the agent can map that GPU onto that Pod and:
//
//  1. attest the sandbox: verify the GPU is healthy, CUDA-capable, and carries
//     no residual processes, then stamp ignition.io/gpu-uuid +
//     ignition.io/init-healthy onto the Pod. The controller requires those
//     before the sandbox becomes public READY.
//  2. verify reuse: once the sandbox Pod is gone, re-check the GPU. If it cannot
//     be proven clean, annotate the Node ignition.io/gpu-cleanup=ambiguous so
//     the controller cordons it and GKE recreates it fresh.
//
// The agent never consults NVIDIA_VISIBLE_DEVICES or device-node names.
package gpuagent

import (
	"context"
	"log"
	"time"

	"ignition.dev/ignition/internal/gpuid"
	"ignition.dev/ignition/internal/k8s"
)

// PodClient is the subset of the Kubernetes pod surface the agent needs.
// *k8s.Cluster and *k8s.Fake both satisfy it.
type PodClient interface {
	ListPodsOnNode(nodeName string) ([]k8s.Pod, error)
	PatchAnnotations(name string, annotations map[string]string) error
}

// NodeMarker sets or clears the reuse-cleanup annotation on the agent's node.
type NodeMarker interface {
	MarkNodeGPUCleanup(nodeName string, ambiguous bool) error
}

// Metrics receives agent outcomes. cmd/ wires a Prometheus implementation;
// internal/ stays free of that dependency.
type Metrics interface {
	ObserveProbe(d time.Duration)
	SetHealthy(ok bool)
	IncAttested()
	IncMarkedDirty(reason string)
}

type nopMetrics struct{}

func (nopMetrics) ObserveProbe(time.Duration) {}
func (nopMetrics) SetHealthy(bool)            {}
func (nopMetrics) IncAttested()               {}
func (nopMetrics) IncMarkedDirty(string)      {}

// Agent reconciles one node's GPU against the sandbox Pod that may run there.
type Agent struct {
	NodeName  string
	Pods      PodClient
	Nodes     NodeMarker
	Inspector Inspector
	Now       func() time.Time
	Metrics   Metrics

	firstPass  bool
	sawSandbox bool
}

// New returns an Agent with defaults filled in.
func New(nodeName string, pods PodClient, nodes NodeMarker, insp Inspector) *Agent {
	return &Agent{
		NodeName:  nodeName,
		Pods:      pods,
		Nodes:     nodes,
		Inspector: insp,
		Now:       time.Now,
		Metrics:   nopMetrics{},
		firstPass: true,
	}
}

// Reconcile runs one pass. It is level-triggered and idempotent: a Pod that is
// already attested is left alone, and the node annotation is only patched when
// its value needs to change.
func (a *Agent) Reconcile(ctx context.Context) error {
	first := a.firstPass
	a.firstPass = false

	pods, err := a.Pods.ListPodsOnNode(a.NodeName)
	if err != nil {
		return err
	}
	sandbox := liveSandbox(pods)
	if sandbox != nil {
		a.sawSandbox = true
		return a.attest(ctx, sandbox)
	}

	// No live sandbox. Verify the GPU is clean for the next tenant when a
	// sandbox has just left, or once on cold start (the agent may have missed
	// the teardown while restarting).
	if a.sawSandbox || first {
		a.sawSandbox = false
		return a.verifyReuse(ctx)
	}
	return nil
}

// Loop ticks Reconcile until ctx is cancelled.
func (a *Agent) Loop(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if err := a.Reconcile(ctx); err != nil {
		log.Printf("gpu-agent: %v", err)
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := a.Reconcile(ctx); err != nil {
				log.Printf("gpu-agent: %v", err)
			}
		}
	}
}

// liveSandbox returns the one non-terminating sandbox Pod scheduled on the node,
// or nil. ListPodsOnNode already filters to the gpu-sandbox workload label.
func liveSandbox(pods []k8s.Pod) *k8s.Pod {
	for i := range pods {
		p := &pods[i]
		if p.Deleting || p.Phase == "Succeeded" || p.Phase == "Failed" {
			continue
		}
		return p
	}
	return nil
}

func (a *Agent) attest(ctx context.Context, pod *k8s.Pod) error {
	if gpuid.IsCanonical(pod.Annotations[k8s.AnnotGPUUUID]) &&
		pod.Annotations[k8s.AnnotInitHealthy] == "true" {
		return nil // already attested
	}

	g, procs, err := a.inspect(ctx)
	if err != nil {
		return err
	}
	if g == nil || !gpuid.IsCanonical(g.UUID) {
		a.markDirty("no-canonical-gpu")
		return a.Nodes.MarkNodeGPUCleanup(a.NodeName, true)
	}
	if !HealthOK(*g) {
		a.Metrics.SetHealthy(false)
		a.markDirty("unhealthy-gpu")
		return a.Nodes.MarkNodeGPUCleanup(a.NodeName, true)
	}
	if len(procs) > 0 {
		// The sandbox is Running but not yet READY and exec is not yet
		// reachable, so the tenant cannot have started these — they are
		// residual from a prior lease.
		a.markDirty("residual-processes")
		return a.Nodes.MarkNodeGPUCleanup(a.NodeName, true)
	}

	a.Metrics.SetHealthy(true)
	if err := a.Pods.PatchAnnotations(pod.Name, map[string]string{
		k8s.AnnotGPUUUID:     g.UUID,
		k8s.AnnotInitHealthy: "true",
		k8s.AnnotGPUHealth:   "ok",
	}); err != nil {
		return err
	}
	a.Metrics.IncAttested()
	return nil
}

func (a *Agent) verifyReuse(ctx context.Context) error {
	g, procs, err := a.inspect(ctx)
	if err != nil {
		return err
	}
	dirty := g == nil || !gpuid.IsCanonical(g.UUID) || !HealthOK(*g) || len(procs) > 0
	a.Metrics.SetHealthy(!dirty)
	if dirty {
		reason := "unhealthy-gpu"
		if len(procs) > 0 {
			reason = "residual-processes"
		}
		a.markDirty(reason)
	}
	return a.Nodes.MarkNodeGPUCleanup(a.NodeName, dirty)
}

// inspect runs one nvidia-smi inventory + compute-process query and returns the
// single GPU (nil when the count is not exactly one).
func (a *Agent) inspect(ctx context.Context) (*GPU, []ComputeProc, error) {
	start := a.Now()
	gpus, err := a.Inspector.Inventory(ctx)
	if err != nil {
		return nil, nil, err
	}
	procs, err := a.Inspector.ComputeProcesses(ctx)
	if err != nil {
		return nil, nil, err
	}
	a.Metrics.ObserveProbe(a.Now().Sub(start))
	if len(gpus) != 1 {
		return nil, procs, nil
	}
	return &gpus[0], procs, nil
}

func (a *Agent) markDirty(reason string) {
	a.Metrics.IncMarkedDirty(reason)
	log.Printf("gpu-agent: node %s GPU not fit for lease: %s", a.NodeName, reason)
}
