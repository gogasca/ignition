package api

import (
	"net/http"

	"ignition.dev/ignition/internal/auth"
)

// getDefaultRuntime returns the system-managed default runtime. Its fields fill
// any RuntimeSpec value a CreateSandbox request leaves unset. Read-only; there
// is no sandbox template resource to manage.
func (s *Server) getDefaultRuntime(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if !s.authorize(w, r, project, auth.PermRuntimeGet, false) {
		return
	}
	writeJSON(w, http.StatusOK, s.cfg.EffectiveDefaultRuntime())
}
