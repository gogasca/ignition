package store

import (
	"context"
	"time"
)

// ObservedUpdate is the controller writing public sandbox state.
type ObservedUpdate struct {
	ProjectID string
	SandboxID string
	State     string
	Reason    string
}

// ProcessObserved is init-supervisor / gateway observation of a process.
type ProcessObserved struct {
	ProjectID string
	SandboxID string
	ProcessID string
	State     string
	ExitCode  *int
}

// ControllerStore is the product-state surface used by ignition-controller.
// Implementations must not call the Kubernetes API.
type ControllerStore interface {
	ListSandboxesAll(ctx context.Context) ([]Sandbox, error)
	UpdateObserved(ctx context.Context, in ObservedUpdate) error
	ListProcessesBySandbox(ctx context.Context, projectID, sandboxID string) ([]Process, error)
	UpdateProcessObserved(ctx context.Context, in ProcessObserved) error
	HoldLease(ctx context.Context, holder string, now time.Time, ttl time.Duration) (bool, error)
}
