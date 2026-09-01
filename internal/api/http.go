package api

import (
	"context"
	"net/http"

	"ignition.dev/ignition/internal/id"
)

// authMiddleware sets the request id, then authenticates every request except
// GET /healthz. It runs outside the adminz metrics middleware so a rejected
// request is counted once (as an auth rejection) rather than as a routed call.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := sanitizeRequestID(r.Header.Get("X-Request-Id"))
		if rid == "" {
			rid = id.New("req")
		}
		w.Header().Set("X-Request-Id", rid)
		ctx := context.WithValue(r.Context(), ctxRequestID, rid)
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		p, err := s.authn.Authenticate(ctx, r.Header.Get("Authorization"))
		if err != nil {
			if s.metrics != nil {
				s.metrics.AuthRejected()
			}
			writeStatus(w, rid, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required", false, 0)
			return
		}
		ctx = context.WithValue(ctx, ctxPrincipal, p)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
