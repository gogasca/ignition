package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"ignition.dev/ignition/internal/id"
)

type Memory struct {
	mu sync.Mutex

	roles       map[string]string // project\x1fsubject -> role
	images      map[string]string // project\x1fimage -> READY
	sandboxes   map[string]Sandbox
	operations  map[string]Operation
	processes   map[string]Process
	idem        map[string]idemRecord
	quotaActive map[string]int
	leaseHolder string
	leaseUntil  time.Time
}

type idemRecord struct {
	Hash     string
	Status   int
	Body     []byte
	Done     bool
	Resource string
}

func NewMemory() *Memory {
	return &Memory{
		roles:       map[string]string{},
		images:      map[string]string{},
		sandboxes:   map[string]Sandbox{},
		operations:  map[string]Operation{},
		processes:   map[string]Process{},
		idem:        map[string]idemRecord{},
		quotaActive: map[string]int{},
	}
}

func bindKey(projectID, subject string) string { return projectID + "\x1f" + subject }
func imgKey(projectID, imageID string) string  { return projectID + "\x1f" + imageID }
func idemSlot(principal, project, method, route, key string) string {
	return principal + "\x1f" + project + "\x1f" + method + "\x1f" + route + "\x1f" + key
}

func (m *Memory) SeedRole(projectID, subject, role string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.roles[bindKey(projectID, subject)] = role
}

func (m *Memory) SeedImage(projectID, imageID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.images[imgKey(projectID, imageID)] = "READY"
}

func (m *Memory) SeedSandbox(sb Sandbox) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sandboxes[sb.ID] = sb
}

func (m *Memory) SetSandboxState(projectID, sandboxID, state string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sb, ok := m.sandboxes[sandboxID]
	if !ok || sb.ProjectID != projectID {
		return
	}
	sb.State = state
	if state == "READY" {
		now := time.Now().UTC()
		sb.ReadyTime = &now
		sb.StateReason = "READY"
	}
	m.sandboxes[sandboxID] = sb
}

func (m *Memory) ResolveRole(_ context.Context, projectID, subject, domain string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if role, ok := m.roles[bindKey(projectID, subject)]; ok {
		return role, true, nil
	}
	if domain != "" {
		if role, ok := m.roles[bindKey(projectID, DomainSubject(domain))]; ok {
			return role, true, nil
		}
	}
	return "", false, nil
}

func (m *Memory) PutRoleBinding(_ context.Context, projectID, subject, role string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.roles[bindKey(projectID, subject)] = role
	return nil
}

func (m *Memory) DeleteRoleBinding(_ context.Context, projectID, subject string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := bindKey(projectID, subject)
	if _, ok := m.roles[key]; !ok {
		return false, nil
	}
	delete(m.roles, key)
	return true, nil
}

func (m *Memory) ListRoleBindings(_ context.Context, projectID string) ([]RoleBinding, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prefix := projectID + "\x1f"
	var out []RoleBinding
	for key, role := range m.roles {
		if strings.HasPrefix(key, prefix) {
			out = append(out, RoleBinding{Subject: strings.TrimPrefix(key, prefix), Role: role})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Subject < out[j].Subject })
	return out, nil
}

func (m *Memory) Ping(_ context.Context) error { return nil }

func (m *Memory) Idempotent(_ context.Context, in IdempotentInput, fn func() (status int, body []byte, err error)) (*IdempotencyReplay, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	slot := idemSlot(in.Principal, in.ProjectID, in.Method, in.Route, in.Key)
	if replay, err := m.lookupIdem(slot, in.Hash); err != nil {
		return nil, err
	} else if replay != nil {
		return replay, nil
	}
	m.idem[slot] = idemRecord{Hash: in.Hash, Done: false}
	status, body, err := fn()
	if err != nil {
		delete(m.idem, slot)
		return nil, err
	}
	m.finishIdem(slot, in.Hash, status, body)
	return &IdempotencyReplay{Status: status, Body: body}, nil
}

func (m *Memory) lookupIdem(slot string, hash string) (*IdempotencyReplay, error) {
	rec, ok := m.idem[slot]
	if !ok {
		return nil, nil
	}
	if rec.Hash != hash {
		return nil, ErrIdempotencyReused
	}
	if !rec.Done {
		return nil, ErrIdempotencyInProgress
	}
	return &IdempotencyReplay{Status: rec.Status, Body: rec.Body}, nil
}

func (m *Memory) finishIdem(slot, hash string, status int, body []byte) {
	m.idem[slot] = idemRecord{Hash: hash, Status: status, Body: body, Done: true}
}

func (m *Memory) CreateSandbox(_ context.Context, in CreateSandboxInput) (CreateSandboxResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	slot := idemSlot(in.Principal, in.ProjectID, "POST", "/sandboxes", in.IdemKey)
	if replay, err := m.lookupIdem(slot, in.IdemHash); err != nil {
		return CreateSandboxResult{}, err
	} else if replay != nil {
		return CreateSandboxResult{Replay: replay}, nil
	}
	m.idem[slot] = idemRecord{Hash: in.IdemHash, Done: false}

	if m.images[imgKey(in.ProjectID, in.ImageID)] != "READY" {
		delete(m.idem, slot)
		return CreateSandboxResult{}, ErrImageNotReady
	}
	max := in.MaxActive
	if max <= 0 {
		max = 100
	}
	if m.quotaActive[in.ProjectID] >= max {
		delete(m.idem, slot)
		return CreateSandboxResult{}, ErrQuotaExceeded
	}

	now := time.Now().UTC()
	sbID := id.New("sbx")
	opID := id.New("op")
	sb := Sandbox{
		ID:          sbID,
		ProjectID:   in.ProjectID,
		Name:        in.Name,
		State:       "CREATING",
		StateReason: "ADMITTED",
		ImageID:     in.ImageID,
		OperationID: opID,
		Generation:  1,
		CreateTime:  now,
		CreatedBy:   in.Principal,
		Command:     in.Command,
		WorkingDir:  in.WorkingDir,
		Resources:   in.Resources,
		Placement:   in.Placement,
		Timeouts:    in.Timeouts,
		Network:     in.Network,
		Labels:      in.Labels,
		SecretRefs:  in.SecretRefs,
	}
	if sb.SecretRefs == nil {
		sb.SecretRefs = []SecretRef{}
	}
	op := Operation{
		ID:         opID,
		ProjectID:  in.ProjectID,
		Kind:       "CREATE_SANDBOX",
		State:      "PENDING",
		ResourceID: sbID,
		CreateTime: now,
		TraceID:    in.TraceID,
		CreatedBy:  in.Principal,
	}
	m.sandboxes[sbID] = sb
	m.operations[opID] = op
	m.quotaActive[in.ProjectID]++

	body, _ := json.Marshal(map[string]any{"sandbox": sb, "operation": op})
	m.finishIdem(slot, in.IdemHash, 202, body)
	return CreateSandboxResult{Sandbox: sb, Operation: op}, nil
}

func (m *Memory) GetSandbox(_ context.Context, projectID, sandboxID string) (Sandbox, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sb, ok := m.sandboxes[sandboxID]
	if !ok || sb.ProjectID != projectID {
		return Sandbox{}, ErrNotFound
	}
	return sb, nil
}

func (m *Memory) ListSandboxes(_ context.Context, projectID string, pageSize int, pageToken string) ([]Sandbox, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var all []Sandbox
	for _, sb := range m.sandboxes {
		if sb.ProjectID == projectID {
			all = append(all, sb)
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].CreateTime.Equal(all[j].CreateTime) {
			return all[i].ID < all[j].ID
		}
		return all[i].CreateTime.Before(all[j].CreateTime)
	})
	return page(all, pageSize, pageToken, func(s Sandbox) string { return s.ID })
}

func (m *Memory) TerminateSandbox(_ context.Context, projectID, sandboxID, principal, idemKey, idemHash, traceID string) (TerminateResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	slot := idemSlot(principal, projectID, "POST", "/sandboxes/"+sandboxID+":terminate", idemKey)
	if replay, err := m.lookupIdem(slot, idemHash); err != nil {
		return TerminateResult{}, err
	} else if replay != nil {
		return TerminateResult{Replay: replay}, nil
	}

	sb, ok := m.sandboxes[sandboxID]
	if !ok || sb.ProjectID != projectID {
		return TerminateResult{}, ErrNotFound
	}

	now := time.Now().UTC()
	op := Operation{
		ID:         id.New("op"),
		ProjectID:  projectID,
		Kind:       "TERMINATE_SANDBOX",
		State:      "PENDING",
		ResourceID: sb.ID,
		CreateTime: now,
		TraceID:    traceID,
		CreatedBy:  principal,
	}
	prev := sb.State
	switch prev {
	case "FINISHED", "FAILED":
		op.State = "SUCCEEDED"
		op.EndTime = &now
	case "TERMINATING":
		// already draining
	default:
		sb.State = "TERMINATING"
		sb.StateReason = "TERMINATE_REQUESTED"
		sb.OperationID = op.ID
		if prev == "CREATING" || prev == "SCHEDULED" || prev == "STARTED" || prev == "READY" {
			m.quotaActive[projectID]--
			if m.quotaActive[projectID] < 0 {
				m.quotaActive[projectID] = 0
			}
		}
	}
	m.sandboxes[sandboxID] = sb
	m.operations[op.ID] = op
	body, _ := json.Marshal(map[string]any{"sandbox": sb, "operation": op})
	m.finishIdem(slot, idemHash, 202, body)
	return TerminateResult{Sandbox: sb, Operation: op}, nil
}

func (m *Memory) GetOperation(_ context.Context, projectID, operationID string) (Operation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	op, ok := m.operations[operationID]
	if !ok || op.ProjectID != projectID {
		return Operation{}, ErrNotFound
	}
	return op, nil
}

func (m *Memory) ListOperations(_ context.Context, projectID string, pageSize int, pageToken, resourceID string) ([]Operation, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var all []Operation
	for _, op := range m.operations {
		if op.ProjectID != projectID {
			continue
		}
		if resourceID != "" && op.ResourceID != resourceID {
			continue
		}
		all = append(all, op)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].CreateTime.Equal(all[j].CreateTime) {
			return all[i].ID < all[j].ID
		}
		return all[i].CreateTime.Before(all[j].CreateTime)
	})
	return page(all, pageSize, pageToken, func(o Operation) string { return o.ID })
}

func (m *Memory) CancelOperation(_ context.Context, projectID, operationID, principal, key, hash string) (Operation, *IdempotencyReplay, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	slot := idemSlot(principal, projectID, "POST", "/operations/"+operationID+":cancel", key)
	if replay, err := m.lookupIdem(slot, hash); err != nil {
		return Operation{}, nil, err
	} else if replay != nil {
		return Operation{}, replay, nil
	}
	op, ok := m.operations[operationID]
	if !ok || op.ProjectID != projectID {
		return Operation{}, nil, ErrNotFound
	}
	now := time.Now().UTC()
	if op.State == "PENDING" || op.State == "RUNNING" {
		op.State = "CANCELLED"
		op.EndTime = &now
		m.operations[operationID] = op
		if op.Kind == "CREATE_SANDBOX" {
			if sb, ok := m.sandboxes[op.ResourceID]; ok && sb.ProjectID == projectID && countsQuota(sb.State) {
				sb.State = "FAILED"
				sb.StateReason = "CANCELLED"
				sb.FinishTime = &now
				m.sandboxes[sb.ID] = sb
				m.quotaActive[projectID]--
				if m.quotaActive[projectID] < 0 {
					m.quotaActive[projectID] = 0
				}
			}
		}
	}
	body, _ := json.Marshal(op)
	m.finishIdem(slot, hash, 200, body)
	return op, nil, nil
}

func (m *Memory) CreateProcess(_ context.Context, in CreateProcessInput) (Process, *IdempotencyReplay, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	slot := idemSlot(in.Principal, in.ProjectID, "POST", "/sandboxes/"+in.SandboxID+"/processes", in.IdemKey)
	if replay, err := m.lookupIdem(slot, in.IdemHash); err != nil {
		return Process{}, nil, err
	} else if replay != nil {
		return Process{}, replay, nil
	}
	sb, ok := m.sandboxes[in.SandboxID]
	if !ok || sb.ProjectID != in.ProjectID {
		return Process{}, nil, ErrNotFound
	}
	if sb.State != "READY" {
		return Process{}, nil, ErrFailedPrecondition
	}
	now := time.Now().UTC()
	p := Process{
		ID:               id.New("prc"),
		ProjectID:        in.ProjectID,
		SandboxID:        in.SandboxID,
		State:            "CREATING",
		Command:          in.Command,
		WorkingDirectory: in.WorkingDir,
		Environment:      in.Environment,
		PTY:              in.PTY,
		CreateTime:       now,
		CreatedBy:        in.Principal,
	}
	m.processes[p.ID] = p
	body, _ := json.Marshal(p)
	m.finishIdem(slot, in.IdemHash, 200, body)
	return p, nil, nil
}

func (m *Memory) GetProcess(_ context.Context, projectID, sandboxID, processID string) (Process, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.processes[processID]
	if !ok || p.ProjectID != projectID || p.SandboxID != sandboxID {
		return Process{}, ErrNotFound
	}
	return p, nil
}

func (m *Memory) ListProcesses(_ context.Context, projectID, sandboxID string, pageSize int, pageToken string) ([]Process, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var all []Process
	for _, p := range m.processes {
		if p.ProjectID == projectID && p.SandboxID == sandboxID {
			all = append(all, p)
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].CreateTime.Equal(all[j].CreateTime) {
			return all[i].ID < all[j].ID
		}
		return all[i].CreateTime.Before(all[j].CreateTime)
	})
	return page(all, pageSize, pageToken, func(p Process) string { return p.ID })
}

func (m *Memory) SignalProcess(_ context.Context, projectID, sandboxID, processID, principal, key, hash, signal string) (Process, *IdempotencyReplay, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	slot := idemSlot(principal, projectID, "POST", "/processes/"+processID+":signal", key)
	if replay, err := m.lookupIdem(slot, hash); err != nil {
		return Process{}, nil, err
	} else if replay != nil {
		return Process{}, replay, nil
	}
	p, ok := m.processes[processID]
	if !ok || p.ProjectID != projectID || p.SandboxID != sandboxID {
		return Process{}, nil, ErrNotFound
	}
	if signal == "" {
		return Process{}, nil, ErrInvalidArgument
	}
	p.TerminatingSignal = signal
	m.processes[processID] = p
	body, _ := json.Marshal(p)
	m.finishIdem(slot, hash, 200, body)
	return p, nil, nil
}

func (m *Memory) CancelProcess(_ context.Context, projectID, sandboxID, processID, principal, key, hash string) (Process, *IdempotencyReplay, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	slot := idemSlot(principal, projectID, "POST", "/processes/"+processID+":cancel", key)
	if replay, err := m.lookupIdem(slot, hash); err != nil {
		return Process{}, nil, err
	} else if replay != nil {
		return Process{}, replay, nil
	}
	p, ok := m.processes[processID]
	if !ok || p.ProjectID != projectID || p.SandboxID != sandboxID {
		return Process{}, nil, ErrNotFound
	}
	if p.State != "EXITED" && p.State != "FAILED" {
		p.State = "CANCELLING"
	}
	m.processes[processID] = p
	body, _ := json.Marshal(p)
	m.finishIdem(slot, hash, 200, body)
	return p, nil, nil
}

func page[T any](all []T, pageSize int, pageToken string, idFn func(T) string) ([]T, string, error) {
	if pageSize <= 0 {
		pageSize = 50
	}
	if pageSize > 100 {
		pageSize = 100
	}
	start := 0
	if pageToken != "" {
		raw, err := base64.RawURLEncoding.DecodeString(pageToken)
		if err != nil {
			return nil, "", ErrInvalidArgument
		}
		want := string(raw)
		for i, item := range all {
			if idFn(item) == want {
				start = i + 1
				break
			}
		}
	}
	if start >= len(all) {
		return []T{}, "", nil
	}
	end := start + pageSize
	if end > len(all) {
		end = len(all)
	}
	out := all[start:end]
	next := ""
	if end < len(all) {
		next = base64.RawURLEncoding.EncodeToString([]byte(idFn(all[end-1])))
	}
	return out, next, nil
}

func (m *Memory) ListSandboxesAll(_ context.Context) ([]Sandbox, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Sandbox, 0, len(m.sandboxes))
	for _, sb := range m.sandboxes {
		out = append(out, sb)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreateTime.Before(out[j].CreateTime) })
	return out, nil
}

func (m *Memory) UpdateObserved(_ context.Context, in ObservedUpdate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	sb, ok := m.sandboxes[in.SandboxID]
	if !ok || sb.ProjectID != in.ProjectID {
		return ErrNotFound
	}
	prev := sb.State
	if prev == "FINISHED" || prev == "FAILED" {
		return nil
	}
	now := time.Now().UTC()
	sb.State = in.State
	sb.StateReason = in.Reason
	switch in.State {
	case "READY":
		if sb.ReadyTime == nil {
			sb.ReadyTime = &now
		}
	case "FAILED", "FINISHED":
		sb.FinishTime = &now
		if prev == "CREATING" || prev == "SCHEDULED" || prev == "STARTED" || prev == "READY" {
			m.quotaActive[sb.ProjectID]--
			if m.quotaActive[sb.ProjectID] < 0 {
				m.quotaActive[sb.ProjectID] = 0
			}
		}
	}
	m.sandboxes[in.SandboxID] = sb
	if sb.OperationID == "" {
		return nil
	}
	op, ok := m.operations[sb.OperationID]
	if !ok {
		return nil
	}
	switch in.State {
	case "SCHEDULED", "STARTED":
		if op.StartTime == nil {
			op.StartTime = &now
		}
		if op.State == "PENDING" {
			op.State = "RUNNING"
		}
		op.ProgressMessage = in.Reason
	case "READY":
		op.State = "SUCCEEDED"
		op.EndTime = &now
		op.ProgressMessage = in.Reason
	case "FAILED":
		op.State = "FAILED"
		op.EndTime = &now
		op.ProgressMessage = in.Reason
	case "FINISHED":
		op.State = "SUCCEEDED"
		op.EndTime = &now
		op.ProgressMessage = in.Reason
	}
	m.operations[sb.OperationID] = op
	return nil
}

func (m *Memory) ListProcessesBySandbox(_ context.Context, projectID, sandboxID string) ([]Process, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Process
	for _, p := range m.processes {
		if p.ProjectID == projectID && p.SandboxID == sandboxID {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *Memory) UpdateProcessObserved(_ context.Context, in ProcessObserved) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.processes[in.ProcessID]
	if !ok || p.ProjectID != in.ProjectID || p.SandboxID != in.SandboxID {
		return ErrNotFound
	}
	now := time.Now().UTC()
	p.State = in.State
	switch in.State {
	case "RUNNING", "STARTING":
		if p.StartTime == nil {
			p.StartTime = &now
		}
	case "FAILED", "EXITED":
		if p.ExitTime == nil {
			p.ExitTime = &now
		}
		if in.ExitCode != nil {
			p.ExitCode = in.ExitCode
		}
	}
	m.processes[in.ProcessID] = p
	return nil
}

func (m *Memory) HoldLease(_ context.Context, holder string, now time.Time, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.leaseHolder == "" || m.leaseHolder == holder || !now.Before(m.leaseUntil) {
		m.leaseHolder = holder
		m.leaseUntil = now.Add(ttl)
		return true, nil
	}
	return false, nil
}

func (m *Memory) QuotaActive(projectID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.quotaActive[projectID]
}
