package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

const (
	defaultStreamSecret = "dev-stream-token-secret"
	defaultAccelerator  = "NVIDIA_L4"
)

// Config is process configuration shared by control-plane binaries.
type Config struct {
	Env                 string
	ListenAddr          string
	DatabaseURL         string
	OIDCIssuer          string
	OIDCJWKSURL         string
	OIDCAudience        string
	GatewayURL          string
	StreamTokenSecret   string
	DevBearer           string
	EnabledRegion       string
	AllowedAccelerators []string
	MaxActiveSandboxes  int
	KubeconfigPath      string
	K8sNamespace        string
	MinWarm             int
	MaxWarm             int
	GCPProject          string
	SandboxImagePrefix  string
}

func Load() (Config, error) {
	maxActive := 100
	if v := os.Getenv("IGNITION_MAX_ACTIVE_SANDBOXES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxActive = n
		}
	}
	env := strings.ToLower(strings.TrimSpace(os.Getenv("IGNITION_ENV")))
	maxWarm := atoiEnv("IGNITION_MAX_WARM", 8)
	// Default the warm floor relative to the ceiling so IGNITION_MAX_WARM=0
	// (warm pool disabled) does not collide with the default floor of 1.
	minWarm := atoiEnv("IGNITION_MIN_WARM", min(1, maxWarm))
	secret := strings.TrimSpace(os.Getenv("IGNITION_STREAM_TOKEN_SECRET"))
	if secret == "" && !RequiresDatabase(env) {
		secret = defaultStreamSecret
	}
	cfg := Config{
		Env:               env,
		ListenAddr:        getenv("IGNITION_LISTEN_ADDR", ":8080"),
		DatabaseURL:       strings.TrimSpace(os.Getenv("DATABASE_URL")),
		OIDCIssuer:        strings.TrimSpace(os.Getenv("IGNITION_OIDC_ISSUER")),
		OIDCJWKSURL:       strings.TrimSpace(os.Getenv("IGNITION_OIDC_JWKS_URL")),
		OIDCAudience:      getenv("IGNITION_OIDC_AUDIENCE", "https://api.ignition.dev"),
		GatewayURL:        getenv("IGNITION_GATEWAY_URL", "https://gateway.us-central1.ignition.dev"),
		StreamTokenSecret: secret,
		DevBearer:         strings.TrimSpace(os.Getenv("IGNITION_DEV_BEARER")),
		EnabledRegion:     getenv("IGNITION_REGION", "us-central1"),
		AllowedAccelerators: splitCSV(getenv("IGNITION_ALLOWED_ACCELERATORS",
			getenv("IGNITION_ALLOWED_GPU_TYPES", defaultAccelerator))),
		MaxActiveSandboxes: maxActive,
		KubeconfigPath:     os.Getenv("KUBECONFIG"),
		K8sNamespace:       getenv("IGNITION_K8S_NAMESPACE", "ignition-sandboxes"),
		MinWarm:            minWarm,
		MaxWarm:            maxWarm,
		GCPProject:         strings.TrimSpace(os.Getenv("IGNITION_GCP_PROJECT")),
		SandboxImagePrefix: strings.TrimSpace(os.Getenv("IGNITION_SANDBOX_IMAGE_PREFIX")),
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate fails closed for staging/prod misconfiguration.
func (c Config) Validate() error {
	if c.MinWarm < 0 || c.MaxWarm < 0 || c.MinWarm > c.MaxWarm {
		return fmt.Errorf("IGNITION_MIN_WARM (%d) and IGNITION_MAX_WARM (%d) must satisfy 0 <= min <= max", c.MinWarm, c.MaxWarm)
	}
	// IGNITION_ENV is free-form: only staging/prod/production are database-backed
	// (see RequiresDatabase). Any other label (dev, test, ci, local, a per-branch
	// name, ...) runs the in-memory dev path with no further checks.
	if !RequiresDatabase(c.Env) {
		return nil
	}
	if strings.TrimSpace(c.DatabaseURL) == "" {
		return fmt.Errorf("IGNITION_ENV=%s requires DATABASE_URL; refusing in-memory store", c.Env)
	}
	if c.DevBearer != "" {
		return fmt.Errorf("IGNITION_ENV=%s forbids IGNITION_DEV_BEARER", c.Env)
	}
	if c.StreamTokenSecret == "" || c.StreamTokenSecret == defaultStreamSecret {
		return fmt.Errorf("IGNITION_ENV=%s requires a non-default IGNITION_STREAM_TOKEN_SECRET", c.Env)
	}
	if len(c.StreamTokenSecret) < 32 {
		return fmt.Errorf("IGNITION_STREAM_TOKEN_SECRET must contain at least 32 bytes")
	}
	if c.OIDCIssuer == "" {
		return fmt.Errorf("IGNITION_ENV=%s requires IGNITION_OIDC_ISSUER", c.Env)
	}
	for name, raw := range map[string]string{"IGNITION_OIDC_ISSUER": c.OIDCIssuer, "IGNITION_OIDC_JWKS_URL": c.OIDCJWKSURL, "IGNITION_GATEWAY_URL": c.GatewayURL} {
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			return fmt.Errorf("%s must be an absolute HTTPS URL without user info", name)
		}
	}
	if strings.TrimSpace(c.SandboxImagePrefix) == "" && strings.TrimSpace(c.GCPProject) == "" {
		return fmt.Errorf("IGNITION_ENV=%s requires IGNITION_SANDBOX_IMAGE_PREFIX or IGNITION_GCP_PROJECT", c.Env)
	}
	return nil
}

// RequiresDatabase reports whether this environment must use Cloud SQL.
func RequiresDatabase(env string) bool {
	switch strings.ToLower(strings.TrimSpace(env)) {
	case "staging", "prod", "production":
		return true
	default:
		return false
	}
}

// AcceleratorAllowed reports whether the platform allowlist permits t. An empty
// allowlist permits any defined AcceleratorType.
func (c Config) AcceleratorAllowed(t string) bool {
	if len(c.AllowedAccelerators) == 0 {
		return true
	}
	for _, allowed := range c.AllowedAccelerators {
		if t == allowed {
			return true
		}
	}
	return false
}

// ResolveSandboxImage concatenates Artifact Registry prefix + imageId.
// Production must set IGNITION_SANDBOX_IMAGE_PREFIX or IGNITION_GCP_PROJECT.
func ResolveSandboxImage(imageID, prefix, region, gcpProject string) string {
	if imageID == "" {
		return ""
	}
	prefix = strings.TrimRight(strings.TrimSpace(prefix), "/")
	if prefix == "" {
		gcpProject = strings.TrimSpace(gcpProject)
		if gcpProject == "" {
			return ""
		}
		if region == "" {
			region = "us-central1"
		}
		prefix = region + "-docker.pkg.dev/" + gcpProject + "/sandboxes"
	}
	return prefix + "/" + imageID
}

func atoiEnv(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func splitCSV(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
