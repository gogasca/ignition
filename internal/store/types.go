package store

import "time"

type Sandbox struct {
	ID          string            `json:"id"`
	ProjectID   string            `json:"projectId"`
	Name        string            `json:"name,omitempty"`
	State       string            `json:"state"`
	StateReason string            `json:"stateReason,omitempty"`
	ImageID     string            `json:"imageId"`
	OperationID string            `json:"operationId,omitempty"`
	Generation  int64             `json:"generation"`
	CreateTime  time.Time         `json:"createTime"`
	ReadyTime   *time.Time        `json:"readyTime,omitempty"`
	FinishTime  *time.Time        `json:"finishTime,omitempty"`
	CreatedBy   string            `json:"-"`
	Command     []string          `json:"-"`
	WorkingDir  string            `json:"-"`
	Environment map[string]string `json:"-"`
	Resources   ResourceSpec      `json:"resources"`
	Placement   PlacementSpec     `json:"placement"`
	Timeouts    TimeoutSpec       `json:"timeouts"`
	Network     NetworkSpec       `json:"network"`
	Labels      map[string]string `json:"labels,omitempty"`
	SecretRefs  []SecretRef       `json:"-"`
}

type SecretRef struct {
	SecretID        string `json:"secretId"`
	Version         string `json:"version,omitempty"`
	EnvironmentName string `json:"environmentName"`
}

type ResourceSpec struct {
	CPUMilli  int     `json:"cpuMilli"`
	MemoryMiB int     `json:"memoryMiB"`
	GPU       GPUSpec `json:"gpu"`
}

type GPUSpec struct {
	Count int    `json:"count"`
	Type  string `json:"type"`
}

type PlacementSpec struct {
	Region                 string `json:"region"`
	ProvisioningPreference string `json:"provisioningPreference,omitempty"`
}

type TimeoutSpec struct {
	StartupSeconds          int `json:"startupSeconds"`
	MaximumRuntimeSeconds   int `json:"maximumRuntimeSeconds"`
	IdleSeconds             int `json:"idleSeconds"`
	TerminationGraceSeconds int `json:"terminationGraceSeconds"`
}

type NetworkSpec struct {
	Egress EgressSpec `json:"egress"`
}

type EgressSpec struct {
	Mode              string   `json:"mode"`
	AllowedTLSDomains []string `json:"allowedTlsDomains,omitempty"`
	AllowedCIDRs      []string `json:"allowedCidrs,omitempty"`
}

type Operation struct {
	ID              string     `json:"id"`
	ProjectID       string     `json:"projectId"`
	Kind            string     `json:"kind"`
	State           string     `json:"state"`
	ResourceID      string     `json:"resourceId"`
	CreateTime      time.Time  `json:"createTime"`
	StartTime       *time.Time `json:"startTime,omitempty"`
	EndTime         *time.Time `json:"endTime,omitempty"`
	TraceID         string     `json:"traceId,omitempty"`
	ProgressMessage string     `json:"progressMessage,omitempty"`
	CreatedBy       string     `json:"-"`
}

type Process struct {
	ID                string            `json:"id"`
	ProjectID         string            `json:"projectId"`
	SandboxID         string            `json:"sandboxId"`
	State             string            `json:"state"`
	Command           []string          `json:"command"`
	WorkingDirectory  string            `json:"workingDirectory,omitempty"`
	Environment       map[string]string `json:"environment,omitempty"`
	PTY               bool              `json:"pty"`
	CreateTime        time.Time         `json:"createTime"`
	StartTime         *time.Time        `json:"startTime,omitempty"`
	ExitTime          *time.Time        `json:"exitTime,omitempty"`
	ExitCode          *int              `json:"exitCode,omitempty"`
	TerminatingSignal string            `json:"terminatingSignal,omitempty"`
	CreatedBy         string            `json:"-"`
}

type CreateSandboxInput struct {
	ProjectID   string
	Principal   string
	IdemKey     string
	IdemHash    string
	Name        string
	ImageID     string
	Command     []string
	WorkingDir  string
	Environment map[string]string
	Resources   ResourceSpec
	Placement   PlacementSpec
	Timeouts    TimeoutSpec
	Network     NetworkSpec
	Labels      map[string]string
	SecretRefs  []SecretRef
	TraceID     string
	MaxActive   int
}

type CreateProcessInput struct {
	ProjectID   string
	SandboxID   string
	Principal   string
	IdemKey     string
	IdemHash    string
	Command     []string
	WorkingDir  string
	Environment map[string]string
	PTY         bool
}

type IdempotencyReplay struct {
	Status int
	Body   []byte
}

type IdempotentInput struct {
	Principal string
	ProjectID string
	Method    string
	Route     string
	Key       string
	Hash      string
}

type CreateSandboxResult struct {
	Sandbox   Sandbox
	Operation Operation
	Replay    *IdempotencyReplay
}

type TerminateResult struct {
	Sandbox   Sandbox
	Operation Operation
	Replay    *IdempotencyReplay
}
