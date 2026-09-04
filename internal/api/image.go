package api

import (
	"fmt"
	"net/http"

	"ignition.dev/ignition/internal/auth"
	"ignition.dev/ignition/internal/store"
)

type createImageBody struct {
	ImageID   string `json:"imageId"`
	SourceRef string `json:"sourceRef"`
}

// createImage admits an arbitrary OCI image reference: it resolves sourceRef
// to an immutable digest against the source registry (s.resolver) and pins
// imageId to that digest. Resolution runs synchronously in this request —
// the "RESOLVING" transient state the catalog schema and design of record
// describe is not observable in v0; a slow or unreachable registry makes
// this call slow or fail rather than returning early and polling. There is
// no Idempotency-Key: a retry after a successful create fails closed with
// IMAGE_ALREADY_EXISTS (imageId is immutable) rather than silently
// re-resolving or duplicating a row.
func (s *Server) createImage(w http.ResponseWriter, r *http.Request) {
	rid := s.requestID(r.Context())
	project := r.PathValue("project")
	if !s.authorize(w, r, project, auth.PermImageCreate, false) {
		return
	}
	raw, err := readBody(w, r, 1<<16)
	if err != nil {
		writeStatus(w, rid, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error(), false, 0)
		return
	}
	var body createImageBody
	if err := decodeJSON(raw, &body); err != nil {
		writeStatus(w, rid, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid JSON", false, 0)
		return
	}
	if body.ImageID == "" {
		writeStatus(w, rid, http.StatusBadRequest, "INVALID_ARGUMENT", "imageId is required", false, 0)
		return
	}
	if err := store.CheckImageID(body.ImageID); err != nil {
		writeStatus(w, rid, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error(), false, 0)
		return
	}
	if err := checkSourceRef(body.SourceRef); err != nil {
		writeStatus(w, rid, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error(), false, 0)
		return
	}

	resolved, err := s.resolver.Resolve(r.Context(), body.SourceRef)
	if err != nil {
		writeStatus(w, rid, http.StatusBadRequest, "IMAGE_UNAVAILABLE", fmt.Sprintf("could not resolve sourceRef: %v", err), false, 0)
		return
	}

	img, err := s.store.CreateImage(r.Context(), store.CreateImageInput{
		ProjectID:         project,
		ImageID:           body.ImageID,
		SourceRef:         body.SourceRef,
		Digest:            resolved.Digest,
		RegistryRef:       resolved.RegistryRef,
		Entrypoint:        resolved.Entrypoint,
		Cmd:               resolved.Cmd,
		StreamingEligible: resolved.StreamingEligible,
		IneligibleReason:  resolved.IneligibleReason,
	})
	if err != nil {
		writeStoreError(w, rid, err)
		return
	}
	w.Header().Set("Location", fmt.Sprintf("/v1/projects/%s/images/%s", project, img.ImageID))
	writeJSON(w, http.StatusCreated, img)
}

func (s *Server) getImage(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if !s.authorize(w, r, project, auth.PermImageGet, false) {
		return
	}
	img, err := s.store.GetImage(r.Context(), project, r.PathValue("image"))
	if err != nil {
		writeStoreError(w, s.requestID(r.Context()), err)
		return
	}
	writeJSON(w, http.StatusOK, img)
}
