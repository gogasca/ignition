package k8s

import (
	"encoding/json"
	"sync"
)

// Fake is an in-memory Pods + Nodes mock. Tests drive kubelet observations
// via SetScheduled / SetRunning / SetReady.
type Fake struct {
	mu        sync.Mutex
	pods      map[string]*Pod
	policies  map[string]*NetworkPolicy
	gpuNodes  map[string]bool
	ScaleDown map[string]bool
	Nodes     []string // CordonAndDelete calls
	Creates   int
	Deletes   int
}

func NewFake() *Fake {
	return &Fake{
		pods:      map[string]*Pod{},
		policies:  map[string]*NetworkPolicy{},
		gpuNodes:  map[string]bool{},
		ScaleDown: map[string]bool{},
	}
}

func clone(p *Pod) *Pod {
	if p == nil {
		return nil
	}
	c := *p
	if p.Labels != nil {
		c.Labels = map[string]string{}
		for k, v := range p.Labels {
			c.Labels[k] = v
		}
	}
	if p.Annotations != nil {
		c.Annotations = map[string]string{}
		for k, v := range p.Annotations {
			c.Annotations[k] = v
		}
	}
	if len(p.Spec.Containers) > 0 {
		c.Spec.Containers = make([]Container, len(p.Spec.Containers))
		copy(c.Spec.Containers, p.Spec.Containers)
		for i, ctr := range p.Spec.Containers {
			if ctr.Env != nil {
				e := map[string]string{}
				for k, v := range ctr.Env {
					e[k] = v
				}
				c.Spec.Containers[i].Env = e
			}
		}
	}
	return &c
}

func (f *Fake) Get(name string) (*Pod, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.pods[name]
	if !ok {
		return nil, ErrNotFound
	}
	return clone(p), nil
}

func (f *Fake) List() ([]Pod, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Pod, 0, len(f.pods))
	for _, p := range f.pods {
		out = append(out, *clone(p))
	}
	return out, nil
}

func (f *Fake) Create(p *Pod) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.pods[p.Name]; ok {
		return ErrAlreadyExists
	}
	f.pods[p.Name] = clone(p)
	f.Creates++
	return nil
}

func (f *Fake) Delete(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.pods[name]; !ok {
		return nil
	}
	delete(f.pods, name)
	f.Deletes++
	return nil
}

func (f *Fake) CordonAndDelete(nodeName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Nodes = append(f.Nodes, nodeName)
	return nil
}

func (f *Fake) PatchAnnotations(name string, annotations map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.pods[name]
	if !ok {
		return ErrNotFound
	}
	if p.Annotations == nil {
		p.Annotations = map[string]string{}
	}
	for k, v := range annotations {
		p.Annotations[k] = v
	}
	return nil
}

func (f *Fake) SetScaleDownDisabled(nodeName string, disabled bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ScaleDown == nil {
		f.ScaleDown = map[string]bool{}
	}
	f.gpuNodes[nodeName] = true
	if disabled {
		f.ScaleDown[nodeName] = true
	} else {
		delete(f.ScaleDown, nodeName)
	}
	return nil
}

func (f *Fake) ListGPUPool() ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := map[string]struct{}{}
	for n := range f.gpuNodes {
		seen[n] = struct{}{}
	}
	for _, p := range f.pods {
		if p.NodeName != "" {
			seen[p.NodeName] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	return out, nil
}

func (f *Fake) ApplyNetworkPolicy(p *NetworkPolicy) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *p
	cp.EgressCIDRs = append([]string{}, p.EgressCIDRs...)
	f.policies[p.Name] = &cp
	return nil
}

func (f *Fake) DeleteNetworkPolicy(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.policies, name)
	return nil
}

func (f *Fake) NetworkPolicy(name string) *NetworkPolicy {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.policies[name]
	if p == nil {
		return nil
	}
	cp := *p
	cp.EgressCIDRs = append([]string{}, p.EgressCIDRs...)
	return &cp
}

func (f *Fake) SetScheduled(name, node string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.pods[name]
	if p == nil {
		return
	}
	p.Scheduled = true
	p.NodeName = node
	p.Phase = "Pending"
	if f.gpuNodes == nil {
		f.gpuNodes = map[string]bool{}
	}
	f.gpuNodes[node] = true
}

func (f *Fake) SetRunning(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.pods[name]
	if p == nil {
		return
	}
	p.Scheduled = true
	p.Running = true
	p.Phase = "Running"
}

func (f *Fake) SetReady(name, gpuUUID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.pods[name]
	if p == nil {
		return
	}
	p.Scheduled = true
	p.Running = true
	p.Ready = true
	p.Phase = "Running"
	if p.Annotations == nil {
		p.Annotations = map[string]string{}
	}
	p.Annotations[AnnotInitHealthy] = "true"
	p.Annotations[AnnotGPUUUID] = gpuUUID
}

func (f *Fake) SetKubeReady(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.pods[name]
	if p == nil {
		return
	}
	p.Scheduled = true
	p.Running = true
	p.Ready = true
	p.Phase = "Running"
}

func (f *Fake) SetProcessObserved(name string, processID, state string, exitCode *int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.pods[name]
	if p == nil {
		return
	}
	if p.Annotations == nil {
		p.Annotations = map[string]string{}
	}
	type rec struct {
		State    string `json:"state"`
		ExitCode *int   `json:"exitCode,omitempty"`
	}
	cur := map[string]rec{}
	if raw := p.Annotations[AnnotProcObserved]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &cur)
	}
	cur[processID] = rec{State: state, ExitCode: exitCode}
	b, _ := json.Marshal(cur)
	p.Annotations[AnnotProcObserved] = string(b)
}

func (f *Fake) SetFailed(name, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.pods[name]
	if p == nil {
		return
	}
	p.Phase = "Failed"
	p.Reason = reason
}

func (f *Fake) SetDeleting(name string, ambiguousGPU bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.pods[name]
	if p == nil {
		return
	}
	p.Deleting = true
	if ambiguousGPU {
		if p.Annotations == nil {
			p.Annotations = map[string]string{}
		}
		p.Annotations[AnnotGPUCleanup] = "ambiguous"
	}
}

func (f *Fake) Drop(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.pods, name)
}

func (f *Fake) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pods)
}
