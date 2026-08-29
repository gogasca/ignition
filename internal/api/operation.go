package api

import (
	"io"
	"net/http"
	"strconv"

	"ignition.dev/ignition/internal/auth"
	"ignition.dev/ignition/internal/store"
)

func (s *Server) getOperation(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if !s.authorize(w, r, project, auth.PermOperationGet, false) {
		return
	}
	op, err := s.store.GetOperation(r.Context(), project, r.PathValue("operation"))
	if err != nil {
		writeStoreError(w, s.requestID(r.Context()), err)
		return
	}
	writeJSON(w, http.StatusOK, op)
}

func (s *Server) listOperations(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if !s.authorize(w, r, project, auth.PermOperationGet, false) {
		return
	}
	size, _ := strconv.Atoi(r.URL.Query().Get("pageSize"))
	items, next, err := s.store.ListOperations(
		r.Context(),
		project,
		size,
		r.URL.Query().Get("pageToken"),
		r.URL.Query().Get("resourceId"),
	)
	if err != nil {
		writeStoreError(w, s.requestID(r.Context()), err)
		return
	}
	if items == nil {
		items = []store.Operation{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"operations": items, "nextPageToken": next})
}

func (s *Server) watchOperation(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if !s.authorize(w, r, project, auth.PermOperationGet, false) {
		return
	}
	op, err := s.store.GetOperation(r.Context(), project, r.PathValue("operation"))
	if err != nil {
		writeStoreError(w, s.requestID(r.Context()), err)
		return
	}
	writeSSE(w, r, op)
}

func (s *Server) cancelOperation(w http.ResponseWriter, r *http.Request) {
	rid := s.requestID(r.Context())
	project := r.PathValue("project")
	operationID := r.PathValue("operation")
	key, ok := requireIdempotency(w, rid, r)
	if !ok {
		return
	}
	op, err := s.store.GetOperation(r.Context(), project, operationID)
	if err != nil {
		if !s.authorize(w, r, project, auth.PermOperationGet, false) {
			return
		}
		writeStoreError(w, rid, err)
		return
	}
	p := s.principal(r.Context())
	own := op.CreatedBy == p.Subject
	if !s.authorizeHidden(w, r, project, auth.PermOperationCancel, own) {
		return
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	out, replay, err := s.store.CancelOperation(r.Context(), project, operationID, p.Subject, key, canonicalHash(r.Method, r.URL.Path, raw))
	if err != nil {
		writeStoreError(w, rid, err)
		return
	}
	if replay != nil {
		writeReplay(w, replay)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
