package store

import "time"

type Sandbox struct {
	ID          string     `json:"id"`
	ProjectID   string     `json:"projectId"`
	Name        string     `json:"name,omitempty"`
	State       string     `json:"state"`
	StateReason string     `json:"stateReason,omitempty"`
	ImageID     string     `json:"imageId"`
	OperationID string     `json:"operationId,omitempty"`
	Generation  int64      `json:"-"`
	CreateTime  time.Time  `json:"createTime"`
	ReadyTime   *time.Time `json:"readyTime,omitempty"`
	FinishTime  *time.Time `json:"finishTime,omitempty"`
	CreatedBy   string     `json:"-"`
	Command     []string   `json:"-"`
	WorkingDir  string     `json:"-"`
	// NativeEntrypoint runs the admitted image's own OCI Entrypoint/Cmd as
	// PID 1 instead of Ignition's managed init supervisor. Set this for an
	// arbitrary/generic image that does not embed sandbox-init: readiness
	// then relies on kubelet's default (container Running, since there is no
	// /readyz to probe), and command/exec/idle-tracking are unavailable for
	// the sandbox.
	NativeEntrypoint bool              `json:"nativeEntrypoint,omitempty"`
	Resources        ResourceSpec      `json:"resources"`
	Placement        PlacementSpec     `json:"placement"`
	Timeouts         TimeoutSpec       `json:"timeouts"`
	Network          NetworkSpec       `json:"network"`
	Labels           map[string]string `json:"labels,omitempty"`
	SecretRefs       []SecretRef       `json:"-"`
}

type SecretRef struct {
	SecretID        string `json:"secretId"`
	Version         string `json:"version,omitempty"`
	EnvironmentName string `json:"environmentName"`
}

// Image is a project-scoped catalog entry: a client-chosen imageId pinned,
// once, to an immutable digest resolved from a source registry reference.
// SourceRef/Digest/RegistryRef/Entrypoint/Cmd are empty for a row created by
// SeedImage (dev/test placeholders) rather than real admission.
type Image struct {
	ProjectID string `json:"projectId"`
	ImageID   string `json:"imageId"`
	// State is "RESOLVING", "READY", or "REJECTED". CreateSandbox admits only
	// "READY". v0 resolves synchronously inside CreateImage, so a client never
	// observes "RESOLVING" for a row it can already GET; the state is kept in
	// the schema because that is the async delivery contract the design of
	// record specifies (see docs/design/ignition-design-image-datalayer.md).
	State       string    `json:"state"`
	StateReason string    `json:"stateReason,omitempty"`
	SourceRef   string    `json:"sourceRef,omitempty"`
	Digest      string    `json:"digest,omitempty"`
	RegistryRef string    `json:"registryRef,omitempty"`
	Entrypoint  []string  `json:"entrypoint,omitempty"`
	Cmd         []string  `json:"cmd,omitempty"`
	CreateTime  time.Time `json:"createTime"`
}

// CreateImageInput is a fully-resolved catalog row to persist. Resolution
// (the registry round trip) happens before this is called; the store layer
// never performs image I/O.
type CreateImageInput struct {
	ProjectID   string
	ImageID     string
	SourceRef   string
	Digest      string
	RegistryRef string
	Entrypoint  []string
	Cmd         []string
}

type ResourceSpec struct {
	CPUMilli  int `json:"cpuMilli"`
	MemoryMiB int `json:"memoryMiB"`
	// Accelerator is the device request. Type "NONE" is a CPU-only sandbox.
	Accelerator AcceleratorSpec `json:"accelerator"`
}

type AcceleratorSpec struct {
	Count int    `json:"count"`
	Type  string `json:"type"`
}

type PlacementSpec struct {
	Region             string `json:"region"`
	ComputeEnvironment string `json:"computeEnvironment"`
}

const (
	ComputeEnvironmentStandard  = "STANDARD"
	ComputeEnvironmentBareMetal = "BARE_METAL"
)

type TimeoutSpec struct {
	StartupSeconds          int `json:"startupSeconds"`
	MaximumRuntimeSeconds   int `json:"maximumRuntimeSeconds"`
	IdleSeconds             int `json:"idleSeconds"`
	TerminationGraceSeconds int `json:"terminationGraceSeconds"`
}

type NetworkSpec struct {
	InternetAccess string `json:"internetAccess"`
}

const (
	InternetAccessDisabled = "DISABLED"
	InternetAccessEnabled  = "ENABLED"
)

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
	ProjectID        string
	Principal        string
	IdemKey          string
	IdemHash         string
	Name             string
	ImageID          string
	Command          []string
	WorkingDir       string
	NativeEntrypoint bool
	Resources        ResourceSpec
	Placement        PlacementSpec
	Timeouts         TimeoutSpec
	Network          NetworkSpec
	Labels           map[string]string
	SecretRefs       []SecretRef
	TraceID          string
	MaxActive        int
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
