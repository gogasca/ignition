package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"ignition.dev/ignition/internal/auth"
	"ignition.dev/ignition/internal/store"
)

type statusBody struct {
	Code              string `json:"code"`
	Message           string `json:"message"`
	RequestID         string `json:"requestId"`
	Retryable         bool   `json:"retryable"`
	RetryAfterSeconds int    `json:"retryAfterSeconds,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeStatus(w http.ResponseWriter, requestID string, httpStatus int, code, message string, retryable bool, retryAfter int) {
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	}
	// Surfaced to clients and read by the adminz request recorder / metrics.
	w.Header().Set("X-Ignition-Error-Code", code)
	writeJSON(w, httpStatus, statusBody{
		Code:              code,
		Message:           message,
		RequestID:         requestID,
		Retryable:         retryable,
		RetryAfterSeconds: retryAfter,
	})
}

func writeStoreError(w http.ResponseWriter, requestID string, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeStatus(w, requestID, http.StatusNotFound, "NOT_FOUND", "not found", false, 0)
	case errors.Is(err, store.ErrIdempotencyReused):
		writeStatus(w, requestID, http.StatusConflict, "IDEMPOTENCY_KEY_REUSED", "idempotency key reused with a different request", false, 0)
	case errors.Is(err, store.ErrIdempotencyInProgress):
		writeStatus(w, requestID, http.StatusConflict, "IDEMPOTENCY_IN_PROGRESS", "duplicate request in progress", true, 1)
	case errors.Is(err, store.ErrImageNotReady):
		writeStatus(w, requestID, http.StatusBadRequest, "IMAGE_NOT_READY", "image is not admitted", false, 0)
	case errors.Is(err, store.ErrQuotaExceeded):
		writeStatus(w, requestID, http.StatusTooManyRequests, "QUOTA_EXCEEDED", "project sandbox quota exceeded", true, 30)
	case errors.Is(err, store.ErrFailedPrecondition):
		writeStatus(w, requestID, http.StatusBadRequest, "FAILED_PRECONDITION", "sandbox is not READY", false, 0)
	case errors.Is(err, store.ErrInvalidArgument):
		writeStatus(w, requestID, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error(), false, 0)
	default:
		writeStatus(w, requestID, http.StatusInternalServerError, "UNAVAILABLE", "internal error", true, 1)
	}
}

func writeReplay(w http.ResponseWriter, replay *store.IdempotencyReplay) {
	if replay == nil {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(replay.Status)
	_, _ = w.Write(replay.Body)
	if len(replay.Body) == 0 || replay.Body[len(replay.Body)-1] != '\n' {
		_, _ = w.Write([]byte("\n"))
	}
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request, projectID string, perm auth.Permission, own bool) bool {
	return s.authorizeResult(w, r, projectID, perm, own, false)
}

func (s *Server) authorizeHidden(w http.ResponseWriter, r *http.Request, projectID string, perm auth.Permission, own bool) bool {
	return s.authorizeResult(w, r, projectID, perm, own, true)
}

func (s *Server) authorizeResult(w http.ResponseWriter, r *http.Request, projectID string, perm auth.Permission, own, hideDeny bool) bool {
	rid := s.requestID(r.Context())
	p := s.principal(r.Context())
	role, ok, err := s.store.ResolveRole(r.Context(), projectID, p.Subject, p.Domain)
	if err != nil {
		writeStoreError(w, rid, err)
		return false
	}
	if !ok {
		writeStatus(w, rid, http.StatusNotFound, "NOT_FOUND", "not found", false, 0)
		return false
	}
	if !auth.Allowed(role, perm, own) {
		if hideDeny {
			writeStatus(w, rid, http.StatusNotFound, "NOT_FOUND", "not found", false, 0)
			return false
		}
		writeStatus(w, rid, http.StatusForbidden, "PERMISSION_DENIED", "permission denied", false, 0)
		return false
	}
	return true
}
