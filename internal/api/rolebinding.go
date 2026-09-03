package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"ignition.dev/ignition/internal/auth"
	"ignition.dev/ignition/internal/store"
)

type roleBindingBody struct {
	Role string `json:"role"`
}

type roleBindingView struct {
	Subject string `json:"subject"`
	Role    string `json:"role"`
}

func (s *Server) listRoleBindings(w http.ResponseWriter, r *http.Request) {
	rid := s.requestID(r.Context())
	project := r.PathValue("project")
	if !s.authorize(w, r, project, auth.PermRoleBindingGet, false) {
		return
	}
	list, err := s.store.ListRoleBindings(r.Context(), project)
	if err != nil {
		writeStoreError(w, rid, err)
		return
	}
	out := make([]roleBindingView, 0, len(list))
	for _, b := range list {
		out = append(out, roleBindingView{Subject: b.Subject, Role: b.Role})
	}
	writeJSON(w, http.StatusOK, map[string]any{"roleBindings": out})
}

func (s *Server) getRoleBinding(w http.ResponseWriter, r *http.Request) {
	rid := s.requestID(r.Context())
	project := r.PathValue("project")
	subject, ok := validRoleBindingSubject(w, rid, r.PathValue("subject"))
	if !ok {
		return
	}
	if !s.authorize(w, r, project, auth.PermRoleBindingGet, false) {
		return
	}
	list, err := s.store.ListRoleBindings(r.Context(), project)
	if err != nil {
		writeStoreError(w, rid, err)
		return
	}
	for _, b := range list {
		if b.Subject == subject {
			writeJSON(w, http.StatusOK, roleBindingView{Subject: b.Subject, Role: b.Role})
			return
		}
	}
	writeStatus(w, rid, http.StatusNotFound, "NOT_FOUND", "role binding not found", false, 0)
}

func (s *Server) putRoleBinding(w http.ResponseWriter, r *http.Request) {
	rid := s.requestID(r.Context())
	project := r.PathValue("project")
	subject, ok := validRoleBindingSubject(w, rid, r.PathValue("subject"))
	if !ok {
		return
	}
	if !s.authorize(w, r, project, auth.PermRoleBindingAdmin, false) {
		return
	}
	raw, err := readBody(w, r, 1<<16)
	if err != nil {
		writeStatus(w, rid, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error(), false, 0)
		return
	}
	var body roleBindingBody
	if err := json.Unmarshal(raw, &body); err != nil {
		writeStatus(w, rid, http.StatusBadRequest, "INVALID_ARGUMENT", "request body is not valid JSON", false, 0)
		return
	}
	role := strings.TrimSpace(body.Role)
	if !auth.KnownRole(role) {
		writeStatus(w, rid, http.StatusBadRequest, "INVALID_ARGUMENT",
			"role must be one of owner, admin, developer, operator, viewer", false, 0)
		return
	}
	orphan, err := s.wouldOrphanProject(r.Context(), project, subject, role)
	if err != nil {
		writeStoreError(w, rid, err)
		return
	}
	if orphan {
		writeStatus(w, rid, http.StatusConflict, "FAILED_PRECONDITION",
			"cannot downgrade the last owner of the project", false, 0)
		return
	}
	if err := s.store.PutRoleBinding(r.Context(), project, subject, role); err != nil {
		writeStoreError(w, rid, err)
		return
	}
	log.Printf("ignition-api: audit rolebinding.put project=%s subject=%s role=%s by=%s rid=%s",
		project, subject, role, s.principal(r.Context()).Subject, rid)
	writeJSON(w, http.StatusOK, roleBindingView{Subject: subject, Role: role})
}

func (s *Server) deleteRoleBinding(w http.ResponseWriter, r *http.Request) {
	rid := s.requestID(r.Context())
	project := r.PathValue("project")
	subject, ok := validRoleBindingSubject(w, rid, r.PathValue("subject"))
	if !ok {
		return
	}
	if !s.authorize(w, r, project, auth.PermRoleBindingAdmin, false) {
		return
	}
	orphan, err := s.wouldOrphanProject(r.Context(), project, subject, "")
	if err != nil {
		writeStoreError(w, rid, err)
		return
	}
	if orphan {
		writeStatus(w, rid, http.StatusConflict, "FAILED_PRECONDITION",
			"cannot remove the last owner of the project", false, 0)
		return
	}
	existed, err := s.store.DeleteRoleBinding(r.Context(), project, subject)
	if err != nil {
		writeStoreError(w, rid, err)
		return
	}
	if !existed {
		writeStatus(w, rid, http.StatusNotFound, "NOT_FOUND", "role binding not found", false, 0)
		return
	}
	log.Printf("ignition-api: audit rolebinding.delete project=%s subject=%s by=%s rid=%s",
		project, subject, s.principal(r.Context()).Subject, rid)
	w.WriteHeader(http.StatusNoContent)
}

// wouldOrphanProject reports whether setting subject to newRole ("" means
// delete) would leave the project with no owner binding.
func (s *Server) wouldOrphanProject(ctx context.Context, project, subject, newRole string) (bool, error) {
	if newRole == auth.RoleOwner {
		return false, nil
	}
	list, err := s.store.ListRoleBindings(ctx, project)
	if err != nil {
		return false, err
	}
	owners, subjectIsOwner := 0, false
	for _, b := range list {
		if b.Role == auth.RoleOwner {
			owners++
			if b.Subject == subject {
				subjectIsOwner = true
			}
		}
	}
	return subjectIsOwner && owners <= 1, nil
}

// validRoleBindingSubject checks the {subject} path segment and normalizes an
// email to lower case. Accepts `email@host.tld` and `domain:<fqdn>`;
// `group:<email>` is reserved but not yet implemented.
func validRoleBindingSubject(w http.ResponseWriter, rid, subject string) (string, bool) {
	subject = strings.TrimSpace(subject)
	if subject == "" || len(subject) > 320 {
		writeStatus(w, rid, http.StatusBadRequest, "INVALID_ARGUMENT", "subject must be 1-320 characters", false, 0)
		return "", false
	}
	if _, isGroup := strings.CutPrefix(subject, "group:"); isGroup {
		writeStatus(w, rid, http.StatusBadRequest, "INVALID_ARGUMENT", "group bindings are not supported yet", false, 0)
		return "", false
	}
	if domain, isDomain := store.TrimDomainSubject(subject); isDomain {
		if domain == "" || !strings.Contains(domain, ".") {
			writeStatus(w, rid, http.StatusBadRequest, "INVALID_ARGUMENT", "domain selector must be domain:<fqdn>", false, 0)
			return "", false
		}
		return subject, true
	}
	at := strings.IndexByte(subject, '@')
	if at <= 0 || !strings.Contains(subject[at+1:], ".") {
		writeStatus(w, rid, http.StatusBadRequest, "INVALID_ARGUMENT", "subject must be an email address or domain:<fqdn>", false, 0)
		return "", false
	}
	return strings.ToLower(subject), true
}
