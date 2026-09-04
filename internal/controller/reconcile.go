package controller

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"time"

	"ignition.dev/ignition/internal/k8s"
	"ignition.dev/ignition/internal/store"
)

func rank(state string) int {
	switch state {
	case "CREATING":
		return 1
	case "SCHEDULED":
		return 2
	case "STARTED":
		return 3
	case "READY":
		return 4
	default:
		return 0
	}
}

func observe(p *k8s.Pod, gpu bool) string {
	if podReady(p, gpu) {
		return "READY"
	}
	if p.Running {
		return "STARTED"
	}
	if p.Scheduled {
		return "SCHEDULED"
	}
	return "CREATING"
}

// podReady reports whether a sandbox Pod may become public READY.
//
// CPU sandboxes: kubelet PodReady (backed by sandbox-init /readyz) is the whole
// signal. GPU sandboxes additionally require ignition-gpu-agent's attestation —
// a canonical GPU UUID plus init-healthy=true — because sandbox-init's local
// probe cannot prove GPU identity or that the card carries no residual
// processes from a prior tenant. The sandbox Pod holds no Kubernetes credential,
// so only the agent can write these annotations.
func podReady(p *k8s.Pod, gpu bool) bool {
	if !gpu {
		return p.Ready
	}
	return p.Ready &&
		p.Annotations[k8s.AnnotInitHealthy] == "true" &&
		k8s.IsCanonicalGPUUUID(p.Annotations[k8s.AnnotGPUUUID])
}

func imagePullFailed(p *k8s.Pod) bool {
	switch p.Reason {
	case "ErrImagePull", "ImagePullBackOff", "InvalidImageName":
		return true
	default:
		return false
	}
}

// cordonIfGPUDirty cordons the Pod's node (GKE then recreates it) when the GPU
// cannot be proven clean for the next tenant. The signal is either a stale Pod
// annotation or, authoritatively, the ignition.io/gpu-cleanup=ambiguous
// annotation ignition-gpu-agent writes on the Node after a failed reuse check.
func (c *Controller) cordonIfGPUDirty(pod *k8s.Pod) error {
	if c.nodes == nil || pod == nil || pod.NodeName == "" {
		return nil
	}
	dirty := pod.Annotations[k8s.AnnotGPUCleanup] == k8s.GPUCleanupAmbiguous
	if !dirty {
		ambiguous, err := c.nodes.GPUCleanupAmbiguous(pod.NodeName)
		if err != nil {
			return err
		}
		dirty = ambiguous
	}
	if !dirty {
		return nil
	}
	return c.nodes.CordonAndDelete(pod.NodeName)
}

// taintReusePending closes the GPU-reuse scheduling window the instant the
// controller deletes a GPU sandbox Pod: the container starts with the GPU
// bound as soon as it is scheduled, before ignition-gpu-agent's health and
// residual-process check has run, so a freed node must not be immediately
// schedulable for a new tenant. Best-effort and non-fatal to the reconcile
// pass — gpuagent.Agent.verifyReuse applies the same taint independently on
// every reuse check (including teardowns this controller never observed,
// such as WORKER_LOST) and is the primary gate; this call only shrinks the
// window between Pod deletion and the agent's next tick.
func (c *Controller) taintReusePending(pod *k8s.Pod, gpu bool) {
	if !gpu || c.nodes == nil || pod == nil || pod.NodeName == "" {
		return
	}
	if err := c.nodes.SetGPUReusePending(pod.NodeName, true); err != nil {
		log.Printf("controller: node %s: set reuse-pending taint: %v", pod.NodeName, err)
	}
}

func (c *Controller) fail(ctx context.Context, sb store.Sandbox, reason string) error {
	return c.store.UpdateObserved(ctx, store.ObservedUpdate{
		ProjectID: sb.ProjectID,
		SandboxID: sb.ID,
		State:     "FAILED",
		Reason:    reason,
	})
}

// failSandbox first makes every child process terminal. If that write fails,
// leave the sandbox nonterminal so the next reconcile pass retries the whole
// transition instead of orphaning active processes under a terminal sandbox.
func (c *Controller) failSandbox(ctx context.Context, sb store.Sandbox, reason string) error {
	if err := c.failProcesses(ctx, sb); err != nil {
		return err
	}
	return c.fail(ctx, sb, reason)
}

func (c *Controller) observeWrite(ctx context.Context, sb store.Sandbox, state, reason string) error {
	if rank(state) <= rank(sb.State) {
		return nil
	}
	if err := c.store.UpdateObserved(ctx, store.ObservedUpdate{
		ProjectID: sb.ProjectID,
		SandboxID: sb.ID,
		State:     state,
		Reason:    reason,
	}); err != nil {
		return err
	}
	// This is a genuine first-observation of state (guarded by the rank
	// check above), so CreateTime -> now is exactly one startup-stage sample
	// for this sandbox, never a re-observation of an already-reached state.
	if c.opts.Metrics != nil {
		c.opts.Metrics.ObserveStage(state, c.opts.Now().Sub(sb.CreateTime))
	}
	return nil
}

func (c *Controller) reconcileSandbox(ctx context.Context, sb store.Sandbox) error {
	switch sb.Placement.ComputeEnvironment {
	case "", store.ComputeEnvironmentStandard:
		// Empty supports development rows created before computeEnvironment was added.
	case store.ComputeEnvironmentBareMetal:
		switch sb.State {
		case "FINISHED", "FAILED":
			return nil
		case "TERMINATING":
			return c.store.UpdateObserved(ctx, store.ObservedUpdate{
				ProjectID: sb.ProjectID,
				SandboxID: sb.ID,
				State:     "FINISHED",
				Reason:    "TERMINATED",
			})
		default:
			return c.fail(ctx, sb, "COMPUTE_ENVIRONMENT_UNAVAILABLE")
		}
	default:
		return c.fail(ctx, sb, "COMPUTE_ENVIRONMENT_UNAVAILABLE")
	}

	name := k8s.PodName(sb.ID)
	pod, err := c.pods.Get(name)
	missing := errors.Is(err, k8s.ErrNotFound)
	if err != nil && !missing {
		return err
	}

	now := c.opts.Now().UTC()
	gpu := k8s.IsGPUProfile(sb.Resources.Accelerator.Type)
	startup := time.Duration(sb.Timeouts.StartupSeconds) * time.Second
	if startup <= 0 {
		startup = 120 * time.Second
	}
	deadline := sb.CreateTime.Add(startup)

	switch sb.State {
	case "FINISHED", "FAILED":
		// A previous controller version could make the sandbox terminal after a
		// transient process update failure. Keep repairing those rows until all
		// child processes are terminal.
		if err := c.failProcesses(ctx, sb); err != nil {
			return err
		}
		if !missing {
			if err := c.cordonIfGPUDirty(pod); err != nil {
				return err
			}
			c.taintReusePending(pod, gpu)
			return c.pods.Delete(name)
		}
		return nil

	case "TERMINATING":
		if !missing {
			if err := c.cordonIfGPUDirty(pod); err != nil {
				return err
			}
			c.taintReusePending(pod, gpu)
			return c.pods.Delete(name)
		}
		return c.store.UpdateObserved(ctx, store.ObservedUpdate{
			ProjectID: sb.ProjectID,
			SandboxID: sb.ID,
			State:     "FINISHED",
			Reason:    "TERMINATED",
		})
	}

	// CREATING / SCHEDULED / STARTED / READY
	if missing {
		if sb.State == "READY" || sb.State == "STARTED" || sb.State == "SCHEDULED" {
			return c.failSandbox(ctx, sb, "WORKER_LOST")
		}
		if !now.Before(deadline) {
			return c.fail(ctx, sb, "CAPACITY_UNAVAILABLE")
		}
		ref := c.opts.ResolveImage(ctx, sb.ProjectID, sb.ImageID)
		if ref == "" {
			return c.fail(ctx, sb, "IMAGE_UNAVAILABLE")
		}
		if _, ok := k8s.ProfileForNetwork(sb.Resources.Accelerator.Type, sb.Network.InternetAccess == store.InternetAccessEnabled); !ok {
			return c.fail(ctx, sb, "WORKLOAD_NOT_SUPPORTED")
		}
		secretEnv, err := c.resolveSecrets(ctx, sb)
		if err != nil {
			return c.fail(ctx, sb, "SECRET_UNAVAILABLE")
		}
		spec := k8s.SandboxPod(sb, ref)
		k8s.ApplySecretEnv(spec, secretEnv)
		cerr := c.pods.Create(spec)
		if cerr != nil && !errors.Is(cerr, k8s.ErrAlreadyExists) {
			return cerr
		}
		return nil
	}

	if imagePullFailed(pod) {
		return c.failSandbox(ctx, sb, "IMAGE_UNAVAILABLE")
	}

	if pod.Phase == "Failed" {
		return c.failSandbox(ctx, sb, "WORKER_LOST")
	}

	if !now.Before(deadline) && observe(pod, gpu) != "READY" {
		reason := "STARTUP_TIMEOUT"
		if !pod.Scheduled {
			reason = "CAPACITY_UNAVAILABLE"
		}
		if err := c.failProcesses(ctx, sb); err != nil {
			return err
		}
		c.taintReusePending(pod, gpu)
		_ = c.pods.Delete(name)
		return c.fail(ctx, sb, reason)
	}

	next := observe(pod, gpu)
	reason := next
	if next == "READY" {
		reason = "READY"
	}
	if err := c.observeWrite(ctx, sb, next, reason); err != nil {
		return err
	}
	if next == "READY" {
		return c.syncProcesses(ctx, sb, pod)
	}
	return nil
}

type processDesired struct {
	Command          []string          `json:"command,omitempty"`
	WorkingDirectory string            `json:"workingDirectory,omitempty"`
	Environment      map[string]string `json:"environment,omitempty"`
	PTY              bool              `json:"pty,omitempty"`
	Signal           string            `json:"signal,omitempty"`
	Cancel           bool              `json:"cancel,omitempty"`
}

type processObservedRec struct {
	State    string `json:"state"`
	ExitCode *int   `json:"exitCode,omitempty"`
}

func (c *Controller) syncProcesses(ctx context.Context, sb store.Sandbox, pod *k8s.Pod) error {
	procs, err := c.store.ListProcessesBySandbox(ctx, sb.ProjectID, sb.ID)
	if err != nil {
		return err
	}
	desired := map[string]processDesired{}
	for _, p := range procs {
		desired[p.ID] = processDesired{
			Command:          p.Command,
			WorkingDirectory: p.WorkingDirectory,
			Environment:      p.Environment,
			PTY:              p.PTY,
			Signal:           p.TerminatingSignal,
			Cancel:           p.State == "CANCELLING",
		}
	}
	raw, _ := json.Marshal(desired)
	if err := c.pods.PatchAnnotations(pod.Name, map[string]string{
		k8s.AnnotProcDesired: string(raw),
	}); err != nil && !errors.Is(err, k8s.ErrNotFound) {
		return err
	}

	observed := map[string]processObservedRec{}
	if s := pod.Annotations[k8s.AnnotProcObserved]; s != "" {
		_ = json.Unmarshal([]byte(s), &observed)
	}
	for _, p := range procs {
		rec, ok := observed[p.ID]
		if !ok || rec.State == "" {
			continue
		}
		if p.State == "EXITED" || p.State == "FAILED" {
			continue
		}
		switch rec.State {
		case "EXITED", "FAILED":
			if err := c.store.UpdateProcessObserved(ctx, store.ProcessObserved{
				ProjectID: p.ProjectID,
				SandboxID: p.SandboxID,
				ProcessID: p.ID,
				State:     rec.State,
				ExitCode:  rec.ExitCode,
			}); err != nil {
				return err
			}
		case "RUNNING", "STARTING":
			if p.State == "CANCELLING" {
				continue
			}
			if p.State == "CREATING" || p.State == "STARTING" {
				if err := c.store.UpdateProcessObserved(ctx, store.ProcessObserved{
					ProjectID: p.ProjectID,
					SandboxID: p.SandboxID,
					ProcessID: p.ID,
					State:     rec.State,
				}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (c *Controller) failProcesses(ctx context.Context, sb store.Sandbox) error {
	procs, err := c.store.ListProcessesBySandbox(ctx, sb.ProjectID, sb.ID)
	if err != nil {
		return err
	}
	for _, p := range procs {
		if p.State == "EXITED" || p.State == "FAILED" {
			continue
		}
		if err := c.store.UpdateProcessObserved(ctx, store.ProcessObserved{
			ProjectID: p.ProjectID,
			SandboxID: p.SandboxID,
			ProcessID: p.ID,
			State:     "FAILED",
		}); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) resolveSecrets(ctx context.Context, sb store.Sandbox) (map[string]string, error) {
	if len(sb.SecretRefs) == 0 {
		return nil, nil
	}
	if c.opts.Secrets == nil {
		return nil, errors.New("secret resolver is not configured")
	}
	out := map[string]string{}
	for _, ref := range sb.SecretRefs {
		val, err := c.opts.Secrets.Resolve(ctx, ref.SecretID, ref.Version)
		if err != nil {
			return nil, err
		}
		out[ref.EnvironmentName] = val
	}
	return out, nil
}

func (c *Controller) pinSandboxNodes() error {
	if c.nodes == nil {
		return nil
	}
	pods, err := c.pods.List()
	if err != nil {
		return err
	}
	occupied := map[string]struct{}{}
	for _, p := range pods {
		if p.Labels[k8s.LabelWorkload] != k8s.WorkloadSandbox {
			continue
		}
		if p.NodeName == "" || p.Deleting {
			continue
		}
		occupied[p.NodeName] = struct{}{}
	}
	names, err := c.nodes.ListGPUPool()
	if err != nil {
		return err
	}
	for _, n := range names {
		_, busy := occupied[n]
		if err := c.nodes.SetScaleDownDisabled(n, busy); err != nil && !errors.Is(err, k8s.ErrCordonRefused) {
			return err
		}
	}
	return nil
}
