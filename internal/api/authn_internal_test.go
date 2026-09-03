package api

import (
	"context"
	"testing"

	"ignition.dev/ignition/internal/auth"
	"ignition.dev/ignition/internal/config"
	"ignition.dev/ignition/internal/store"
)

func TestBuildOIDCAuthenticator(t *testing.T) {
	primaryOnly := buildOIDCAuthenticator(config.Config{
		OIDCIssuer: config.GoogleOIDCIssuer, OIDCAudience: "https://api", OIDCSubjectClaim: "email",
	})
	j, ok := primaryOnly.(*auth.JWT)
	if !ok {
		t.Fatalf("no-IAP: want *auth.JWT, got %T", primaryOnly)
	}
	if !j.RequireEmailVerified {
		t.Error("email subject claim should require verified email")
	}

	withIAP := buildOIDCAuthenticator(config.Config{
		OIDCIssuer: config.GoogleOIDCIssuer, OIDCAudience: "https://api", OIDCSubjectClaim: "email",
		IAPEnabled: true, IAPIssuer: "https://cloud.google.com/iap",
		IAPAudience: "/projects/1/global/backendServices/2", IAPJWKSURL: "https://iap.example/keys",
	})
	ch, ok := withIAP.(auth.Chain)
	if !ok || len(ch) != 2 {
		t.Fatalf("IAP: want auth.Chain of length 2, got %T", withIAP)
	}
}

func TestBootstrapAdmin(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()

	// Unset: no-op.
	if err := bootstrapAdmin(ctx, mem, config.Config{}); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{BootstrapProject: "prj", BootstrapAdmin: "root@corp.example"}
	if err := bootstrapAdmin(ctx, mem, cfg); err != nil {
		t.Fatal(err)
	}
	if role, ok, _ := mem.ResolveRole(ctx, "prj", "root@corp.example", ""); !ok || role != auth.RoleOwner {
		t.Fatalf("seeded role = %q ok = %v", role, ok)
	}

	// An owner already exists: a second call with a different admin is a no-op.
	cfg.BootstrapAdmin = "other@corp.example"
	if err := bootstrapAdmin(ctx, mem, cfg); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := mem.ResolveRole(ctx, "prj", "other@corp.example", ""); ok {
		t.Fatal("bootstrap must not add a second owner")
	}
}
