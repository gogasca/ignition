package probe

import "time"

// The probe speaks the public v1 JSON API. These structs mirror only the
// fields the journeys assert; unknown fields are ignored on decode. They are
// deliberately decoupled from internal/store so the client stays a thin,
// dependency-light SDK that ignitionctl can also reuse.

// AcceleratorReq is the accelerator block of a create request.
type AcceleratorReq struct {
	Count int    `json:"count"`
	Type  string `json:"type"`
}

// ResourceReq is the resources block of a create request.
type ResourceReq struct {
	CPUMilli    int             `json:"cpuMilli,omitempty"`
	MemoryMiB   int             `json:"memoryMiB,omitempty"`
	Accelerator *AcceleratorReq `json:"accelerator,omitempty"`
}

// NetworkReq is the network block of a create request.
type NetworkReq struct {
	InternetAccess string `json:"internetAccess,omitempty"`
}

// CreateSandboxReq is the POST /sandboxes body.
type CreateSandboxReq struct {
	Name      string       `json:"name,omitempty"`
	ImageID   string       `json:"imageId"`
	Command   []string     `json:"command,omitempty"`
	Resources *ResourceReq `json:"resources,omitempty"`
	Network   *NetworkReq  `json:"network,omitempty"`
}

// SandboxView is a subset of the Sandbox resource.
type SandboxView struct {
	ID          string     `json:"id"`
	ProjectID   string     `json:"projectId"`
	Name        string     `json:"name"`
	State       string     `json:"state"`
	StateReason string     `json:"stateReason"`
	ImageID     string     `json:"imageId"`
	OperationID string     `json:"operationId"`
	CreateTime  time.Time  `json:"createTime"`
	ReadyTime   *time.Time `json:"readyTime"`
	FinishTime  *time.Time `json:"finishTime"`
}

// Terminal reports whether the sandbox is in a terminal or terminating state.
func (s SandboxView) Terminal() bool {
	switch s.State {
	case "FINISHED", "FAILED", "TERMINATING":
		return true
	default:
		return false
	}
}

// OperationView is a subset of the Operation resource.
type OperationView struct {
	ID              string `json:"id"`
	Kind            string `json:"kind"`
	State           string `json:"state"`
	ResourceID      string `json:"resourceId"`
	ProgressMessage string `json:"progressMessage"`
}

// Done reports whether the operation reached a terminal state.
func (o OperationView) Done() bool {
	switch o.State {
	case "SUCCEEDED", "FAILED", "CANCELLED":
		return true
	default:
		return false
	}
}

// ProcessView is a subset of the Process resource.
type ProcessView struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	ExitCode *int   `json:"exitCode"`
}

// AttachView is the AttachProcess response.
type AttachView struct {
	StreamToken string    `json:"streamToken"`
	GatewayURL  string    `json:"gatewayUrl"`
	ExpireTime  time.Time `json:"expireTime"`
	StreamEpoch int64     `json:"streamEpoch"`
}

// createEnvelope is the {sandbox, operation} response shared by create and
// terminate.
type createEnvelope struct {
	Sandbox   SandboxView   `json:"sandbox"`
	Operation OperationView `json:"operation"`
}

// RuntimeView is the GET /runtimes/default response.
type RuntimeView struct {
	Resources struct {
		CPUMilli  int `json:"cpuMilli"`
		MemoryMiB int `json:"memoryMiB"`
	} `json:"resources"`
	Placement struct {
		ComputeEnvironment string `json:"computeEnvironment"`
	} `json:"placement"`
	Network struct {
		InternetAccess string `json:"internetAccess"`
	} `json:"network"`
}

// sandboxList is the GET /sandboxes response.
type sandboxList struct {
	Sandboxes     []SandboxView `json:"sandboxes"`
	NextPageToken string        `json:"nextPageToken"`
}

// sandboxRank orders the happy-path sandbox states so journeys can assert that
// observed state never moves backwards. Mirrors internal/controller.rank;
// terminal and TERMINATING states rank 0 and are handled explicitly by callers.
func sandboxRank(state string) int {
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
