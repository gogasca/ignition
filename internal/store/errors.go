package store

import "errors"

var (
	ErrNotFound              = errors.New("not found")
	ErrIdempotencyReused     = errors.New("idempotency key reused")
	ErrIdempotencyInProgress = errors.New("idempotency in progress")
	ErrImageNotReady         = errors.New("image not ready")
	ErrQuotaExceeded         = errors.New("quota exceeded")
	ErrFailedPrecondition    = errors.New("failed precondition")
	ErrInvalidArgument       = errors.New("invalid argument")
)
