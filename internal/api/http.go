package api

import (
	"context"
	"net/http"

	"ignition.dev/ignition/internal/id"
)

func (s *Server) middleware(next http.Handler) http.Handler {
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
			writeStatus(w, rid, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required", false, 0)
			return
		}
		ctx = context.WithValue(ctx, ctxPrincipal, p)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
