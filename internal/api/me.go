package api

import "net/http"

// getMe echoes the authenticated principal. It needs authentication (the
// middleware already ran) but no project scope or permission — clients use it
// to verify a token and show "logged in as ...". There is no project-list
// endpoint: the Project API is not exposed, so the caller's project context is
// client-side configuration.
func (s *Server) getMe(w http.ResponseWriter, r *http.Request) {
	p := s.principal(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"subject": p.Subject,
		"email":   p.Email,
		"kind":    string(p.Kind),
		"domain":  p.Domain,
	})
}
