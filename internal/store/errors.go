package store

import "errors"

var (
	ErrNotFound              = errors.New("not found")
	ErrIdempotencyReused     = errors.New("idempotency key reused")
	ErrIdempotencyInProgress = errors.New("idempotency in progress")
	ErrImageNotReady         = errors.New("image not ready")
	ErrImageAlreadyExists    = errors.New("imageId already registered in this project")
	ErrSecretNotFound        = errors.New("secret not found in project")
	ErrQuotaExceeded         = errors.New("quota exceeded")
	ErrFailedPrecondition    = errors.New("failed precondition")
	ErrInvalidArgument       = errors.New("invalid argument")
	// ErrLastOwner means the requested role_bindings write would leave the
	// project with no owner (the "last-owner guard" — see
	// docs/design/ignition-api-contract.md §Project RBAC). PutRoleBinding and
	// DeleteRoleBinding enforce this themselves, atomically with the write
	// (a single mutex for Memory, one locked transaction for Postgres) —
	// never as a separate check before the write, which two concurrent
	// requests could each pass before either commits.
	ErrLastOwner = errors.New("cannot remove the project's last owner")
)

// AdmissionError is returned by a CreateSandboxInput.Admit hook to reject a
// creation with a specific client-facing code and message. Because Admit runs
// inside CreateSandbox's idempotent transaction on the non-replay path only,
// carrying the rejection this way keeps a replayed request on its original
// outcome. internal/api renders it as an HTTP 400 with the given code.
type AdmissionError struct {
	Code    string
	Message string
}

func (e *AdmissionError) Error() string { return e.Code + ": " + e.Message }
