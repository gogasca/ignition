package controller

import (
	"context"
	"encoding/json"
	"errors"
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

func observe(p *k8s.Pod) string {
	// PodReady is backed by sandbox-init /readyz, which verifies a single
	// explicit GPU assignment. Legacy annotations remain accepted while old
	// Pods roll out, but sandbox-init never receives Kubernetes credentials.
	if p.Ready || (p.Annotations[k8s.AnnotInitHealthy] == "true" && p.Annotations[k8s.AnnotGPUUUID] != "") {
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

func imagePullFailed(p *k8s.Pod) bool {
	switch p.Reason {
	case "ErrImagePull", "ImagePullBackOff", "InvalidImageName":
		return true
	default:
		return false
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

func (c *Controller) observeWrite(ctx context.Context, sb store.Sandbox, state, reason string) error {
	if rank(state) <= rank(sb.State) {
		return nil
	}
	return c.store.UpdateObserved(ctx, store.ObservedUpdate{
		ProjectID: sb.ProjectID,
		SandboxID: sb.ID,
		State:     state,
		Reason:    reason,
	})
}

func (c *Controller) reconcileSandbox(ctx context.Context, sb store.Sandbox) error {
	name := k8s.PodName(sb.ID)
	pod, err := c.pods.Get(name)
	missing := errors.Is(err, k8s.ErrNotFound)
	if err != nil && !missing {
		return err
	}

	now := c.opts.Now().UTC()
	startup := time.Duration(sb.Timeouts.StartupSeconds) * time.Second
	if startup <= 0 {
		startup = 120 * time.Second
	}
	deadline := sb.CreateTime.Add(startup)

	switch sb.State {
	case "FINISHED", "FAILED":
		if !missing {
			if pod.Annotations[k8s.AnnotGPUCleanup] == "ambiguous" && c.nodes != nil && pod.NodeName != "" {
				if err := c.nodes.CordonAndDelete(pod.NodeName); err != nil {
					return err
				}
			}
			_ = c.deletePolicy(sb.ID)
			return c.pods.Delete(name)
		}
		_ = c.deletePolicy(sb.ID)
		return nil

	case "TERMINATING":
		if !missing {
			if pod.Annotations[k8s.AnnotGPUCleanup] == "ambiguous" && c.nodes != nil && pod.NodeName != "" {
				if err := c.nodes.CordonAndDelete(pod.NodeName); err != nil {
					return err
				}
			}
			_ = c.deletePolicy(sb.ID)
			return c.pods.Delete(name)
		}
		_ = c.deletePolicy(sb.ID)
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
			_ = c.failProcesses(ctx, sb)
			_ = c.deletePolicy(sb.ID)
			return c.fail(ctx, sb, "WORKER_LOST")
		}
		if !now.Before(deadline) {
			return c.fail(ctx, sb, "CAPACITY_UNAVAILABLE")
		}
		ref := c.opts.ResolveImage(sb.ImageID)
		if ref == "" {
			return c.fail(ctx, sb, "IMAGE_UNAVAILABLE")
		}
		if k8s.NodePoolForGPUType(sb.Resources.GPU.Type) == "" {
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
		return c.applyPolicy(sb)
	}

	if imagePullFailed(pod) {
		_ = c.failProcesses(ctx, sb)
		return c.fail(ctx, sb, "IMAGE_UNAVAILABLE")
	}

	if pod.Phase == "Failed" {
		_ = c.failProcesses(ctx, sb)
		return c.fail(ctx, sb, "WORKER_LOST")
	}

	if !now.Before(deadline) && observe(pod) != "READY" {
		reason := "STARTUP_TIMEOUT"
		if !pod.Scheduled {
			reason = "CAPACITY_UNAVAILABLE"
		}
		_ = c.failProcesses(ctx, sb)
		_ = c.deletePolicy(sb.ID)
		_ = c.pods.Delete(name)
		return c.fail(ctx, sb, reason)
	}

	if err := c.applyPolicy(sb); err != nil {
		return err
	}

	next := observe(pod)
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

func (c *Controller) applyPolicy(sb store.Sandbox) error {
	if c.opts.Policies == nil {
		return nil
	}
	return c.opts.Policies.ApplyNetworkPolicy(k8s.SandboxNetworkPolicy(sb))
}

func (c *Controller) deletePolicy(sandboxID string) error {
	if c.opts.Policies == nil {
		return nil
	}
	return c.opts.Policies.DeleteNetworkPolicy(k8s.PolicyName(sandboxID))
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
