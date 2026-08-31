package config_test

import (
	"strings"
	"testing"

	"ignition.dev/ignition/internal/config"
)

func TestLoadStagingRequiresDatabaseURL(t *testing.T) {
	t.Setenv("IGNITION_ENV", "staging")
	t.Setenv("DATABASE_URL", "")
	_, err := config.Load()
	if err == nil || !strings.Contains(err.Error(), "requires DATABASE_URL") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadProdAcceptsDATABASE_URL(t *testing.T) {
	t.Setenv("IGNITION_ENV", "prod")
	t.Setenv("DATABASE_URL", "postgres://ignition@127.0.0.1:5432/ignition")
	t.Setenv("IGNITION_OIDC_ISSUER", "https://issuer.example")
	t.Setenv("IGNITION_STREAM_TOKEN_SECRET", "prod-stream-token-secret-32-bytes!!")
	t.Setenv("IGNITION_DEV_BEARER", "")
	t.Setenv("IGNITION_GCP_PROJECT", "ignition-prod")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabaseURL != "postgres://ignition@127.0.0.1:5432/ignition" {
		t.Fatalf("dsn = %q", cfg.DatabaseURL)
	}
}

func TestLoadProdRejectsDevBearer(t *testing.T) {
	t.Setenv("IGNITION_ENV", "prod")
	t.Setenv("DATABASE_URL", "postgres://ignition@127.0.0.1:5432/ignition")
	t.Setenv("IGNITION_OIDC_ISSUER", "https://issuer.example")
	t.Setenv("IGNITION_STREAM_TOKEN_SECRET", "prod-stream-token-secret-32-bytes!!")
	t.Setenv("IGNITION_DEV_BEARER", "dev-token")
	_, err := config.Load()
	if err == nil || !strings.Contains(err.Error(), "forbids IGNITION_DEV_BEARER") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadProdRejectsDefaultStreamSecret(t *testing.T) {
	t.Setenv("IGNITION_ENV", "staging")
	t.Setenv("DATABASE_URL", "postgres://ignition@127.0.0.1:5432/ignition")
	t.Setenv("IGNITION_OIDC_ISSUER", "https://issuer.example")
	t.Setenv("IGNITION_STREAM_TOKEN_SECRET", "")
	t.Setenv("IGNITION_DEV_BEARER", "")
	_, err := config.Load()
	if err == nil || !strings.Contains(err.Error(), "IGNITION_STREAM_TOKEN_SECRET") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadDevAllowsMissingDatabase(t *testing.T) {
	t.Setenv("IGNITION_ENV", "dev")
	t.Setenv("DATABASE_URL", "")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabaseURL != "" {
		t.Fatalf("dsn = %q", cfg.DatabaseURL)
	}
}

func TestValidateProdWithoutDSN(t *testing.T) {
	err := config.Config{Env: "prod"}.Validate()
	if err == nil || !strings.Contains(err.Error(), "refusing in-memory store") {
		t.Fatalf("err = %v", err)
	}
}

func TestRequiresDatabase(t *testing.T) {
	if !config.RequiresDatabase("staging") || !config.RequiresDatabase("PROD") {
		t.Fatal("staging/prod must require a database")
	}
	if config.RequiresDatabase("dev") || config.RequiresDatabase("sample") || config.RequiresDatabase("") {
		t.Fatal("dev/sample/empty must allow memory")
	}
}

func TestLoadStagingRequiresImageRegistry(t *testing.T) {
	t.Setenv("IGNITION_ENV", "staging")
	t.Setenv("DATABASE_URL", "postgres://ignition@127.0.0.1:5432/ignition")
	t.Setenv("IGNITION_OIDC_ISSUER", "https://issuer.example")
	t.Setenv("IGNITION_STREAM_TOKEN_SECRET", "prod-stream-token-secret-32-bytes!!")
	t.Setenv("IGNITION_DEV_BEARER", "")
	t.Setenv("IGNITION_GCP_PROJECT", "")
	t.Setenv("IGNITION_SANDBOX_IMAGE_PREFIX", "")
	_, err := config.Load()
	if err == nil || !strings.Contains(err.Error(), "IGNITION_SANDBOX_IMAGE_PREFIX") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadMaxWarmZeroDisablesPool(t *testing.T) {
	t.Setenv("IGNITION_ENV", "dev")
	t.Setenv("IGNITION_MAX_WARM", "0")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("MAX_WARM=0 must be accepted: %v", err)
	}
	if cfg.MinWarm != 0 || cfg.MaxWarm != 0 {
		t.Fatalf("warm bounds = %d/%d, want 0/0", cfg.MinWarm, cfg.MaxWarm)
	}
}

func TestLoadRejectsExplicitMinAboveMax(t *testing.T) {
	t.Setenv("IGNITION_ENV", "dev")
	t.Setenv("IGNITION_MIN_WARM", "5")
	t.Setenv("IGNITION_MAX_WARM", "2")
	if _, err := config.Load(); err == nil || !strings.Contains(err.Error(), "IGNITION_MIN_WARM") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadAcceptsCustomEnvLabel(t *testing.T) {
	t.Setenv("IGNITION_ENV", "ci")
	t.Setenv("DATABASE_URL", "")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("custom env label must be accepted: %v", err)
	}
	if cfg.Env != "ci" {
		t.Fatalf("env = %q", cfg.Env)
	}
}

func TestResolveSandboxImage(t *testing.T) {
	got := config.ResolveSandboxImage("img_seed", "us-central1-docker.pkg.dev/my-proj/sandboxes", "us-central1", "")
	if got != "us-central1-docker.pkg.dev/my-proj/sandboxes/img_seed" {
		t.Fatalf("prefix path = %q", got)
	}
	got = config.ResolveSandboxImage("img_seed", "", "us-central1", "acme-dev")
	if got != "us-central1-docker.pkg.dev/acme-dev/sandboxes/img_seed" {
		t.Fatalf("gcp project path = %q", got)
	}
	if config.ResolveSandboxImage("img_seed", "", "us-central1", "") != "" {
		t.Fatal("empty project must fail closed")
	}
}
