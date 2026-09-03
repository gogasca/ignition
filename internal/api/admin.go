package api

import (
	"net/http"
	"strconv"
	"strings"

	"ignition.dev/ignition/internal/adminz"
)

// AdminHandler returns the private admin surface (/healthz, /statusz, /rpcz,
// /metrics). It performs no authentication and must be bound only to
// cfg.AdminAddr, never the public listener or the Ingress.
func (s *Server) AdminHandler() http.Handler {
	return adminz.New(adminz.Options{
		Name:     "ignition-api",
		Registry: s.registry,
		Recorder: s.recorder,
		HealthFn: s.store.Ping,
		Status:   s.statusSections,
	}).Handler()
}

func (s *Server) statusSections() []adminz.Section {
	backend := "in-memory"
	if strings.TrimSpace(s.cfg.DatabaseURL) != "" {
		backend = "postgres"
	}
	authMode := "closed (no tokens)"
	switch {
	case strings.TrimSpace(s.cfg.DevBearer) != "":
		authMode = "dev bearer"
	case s.cfg.OIDCIssuer != "":
		authMode = "oidc: " + s.cfg.OIDCIssuer
	}
	iap := "disabled"
	if s.cfg.IAPEnabled {
		iap = "enabled aud=" + orDash(s.cfg.IAPAudience)
	}
	audiences := append([]string{s.cfg.OIDCAudience}, s.cfg.OIDCAudiences...)
	return []adminz.Section{{
		Title: "api",
		Rows: [][2]string{
			{"env", orDash(s.cfg.Env)},
			{"region", s.cfg.EnabledRegion},
			{"store", backend},
			{"auth", authMode},
			{"oidc audiences", strings.Join(audiences, ", ")},
			{"oidc subject claim", orDash(s.cfg.OIDCSubjectClaim)},
			{"oidc hosted domains", orDash(strings.Join(s.cfg.OIDCHostedDomains, ", "))},
			{"iap", iap},
			{"gateway url", orDash(s.cfg.GatewayURL)},
			{"max active sandboxes", strconv.Itoa(s.cfg.MaxActiveSandboxes)},
			{"allowed accelerators", strings.Join(s.cfg.AllowedAccelerators, ", ")},
		},
	}}
}

func orDash(v string) string {
	if strings.TrimSpace(v) == "" {
		return "-"
	}
	return v
}
