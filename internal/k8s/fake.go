package k8s

import (
	"encoding/json"
	"sync"
)

// Fake is an in-memory Pods + Nodes mock. Tests drive kubelet observations
// via SetScheduled / SetRunning / SetReady.
type Fake struct {
	mu           sync.Mutex
	pods         map[string]*Pod
	gpuNodes     map[string]bool
	dirtyNodes   map[string]bool
	reusePending map[string]bool
	ScaleDown    map[string]bool
	Nodes        []string // CordonAndDelete calls
	Creates      int
	Deletes      int
}

func NewFake() *Fake {
	return &Fake{
		pods:         map[string]*Pod{},
		gpuNodes:     map[string]bool{},
		dirtyNodes:   map[string]bool{},
		reusePending: map[string]bool{},
		ScaleDown:    map[string]bool{},
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

func (f *Fake) GPUCleanupAmbiguous(nodeName string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dirtyNodes[nodeName], nil
}

func (f *Fake) MarkNodeGPUCleanup(nodeName string, ambiguous bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dirtyNodes == nil {
		f.dirtyNodes = map[string]bool{}
	}
	if ambiguous {
		f.dirtyNodes[nodeName] = true
	} else {
		delete(f.dirtyNodes, nodeName)
	}
	return nil
}

// MarkNodeDirty is a test helper: flag a node so the controller cordons it on
// the next sandbox teardown, as ignition-gpu-agent would after a failed reuse
// check.
func (f *Fake) MarkNodeDirty(nodeName string) { _ = f.MarkNodeGPUCleanup(nodeName, true) }

func (f *Fake) SetGPUReusePending(nodeName string, pending bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reusePending == nil {
		f.reusePending = map[string]bool{}
	}
	if pending {
		f.reusePending[nodeName] = true
	} else {
		delete(f.reusePending, nodeName)
	}
	return nil
}

// GPUReusePending is a test accessor for the taint SetGPUReusePending sets.
func (f *Fake) GPUReusePending(nodeName string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reusePending[nodeName]
}

func (f *Fake) ListPodsOnNode(nodeName string) ([]Pod, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Pod
	for _, p := range f.pods {
		if p.NodeName == nodeName && p.Labels[LabelWorkload] == WorkloadSandbox {
			out = append(out, *clone(p))
		}
	}
	return out, nil
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
	// Mirror what ignition-gpu-agent writes: a canonical GPU UUID. Tests pass
	// shorthand like "GPU-1"; normalize it so the controller's READY gate
	// (k8s.IsCanonicalGPUUUID) is satisfied. An empty string is a CPU sandbox.
	if gpuUUID != "" && !IsCanonicalGPUUUID(gpuUUID) {
		gpuUUID = FakeGPUUUID
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
