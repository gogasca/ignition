package store

import "context"

// Store is the product-state layer used by ignition-api.
// Implementations must not call the Kubernetes API.
type Store interface {
	Role(ctx context.Context, projectID, subject string) (role string, ok bool, err error)
	CreateSandbox(ctx context.Context, in CreateSandboxInput) (CreateSandboxResult, error)
	GetSandbox(ctx context.Context, projectID, sandboxID string) (Sandbox, error)
	ListSandboxes(ctx context.Context, projectID string, pageSize int, pageToken string) ([]Sandbox, string, error)
	TerminateSandbox(ctx context.Context, projectID, sandboxID, principal, idemKey, idemHash, traceID string) (TerminateResult, error)
	GetOperation(ctx context.Context, projectID, operationID string) (Operation, error)
	ListOperations(ctx context.Context, projectID string, pageSize int, pageToken, resourceID string) ([]Operation, string, error)
	CancelOperation(ctx context.Context, projectID, operationID, principal, idemKey, idemHash string) (Operation, *IdempotencyReplay, error)
	CreateProcess(ctx context.Context, in CreateProcessInput) (Process, *IdempotencyReplay, error)
	GetProcess(ctx context.Context, projectID, sandboxID, processID string) (Process, error)
	ListProcesses(ctx context.Context, projectID, sandboxID string, pageSize int, pageToken string) ([]Process, string, error)
	SignalProcess(ctx context.Context, projectID, sandboxID, processID, principal, idemKey, idemHash, signal string) (Process, *IdempotencyReplay, error)
	CancelProcess(ctx context.Context, projectID, sandboxID, processID, principal, idemKey, idemHash string) (Process, *IdempotencyReplay, error)
	Idempotent(ctx context.Context, in IdempotentInput, fn func() (status int, body []byte, err error)) (*IdempotencyReplay, error)
	Ping(ctx context.Context) error
}
