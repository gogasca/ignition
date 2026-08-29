package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	defaultStreamSecret = "dev-stream-token-secret"
	defaultGPUType      = "NVIDIA_L4"
)

// Config is process configuration shared by control-plane binaries.
type Config struct {
	Env                string
	ListenAddr         string
	DatabaseURL        string
	OIDCIssuer         string
	OIDCJWKSURL        string
	OIDCAudience       string
	GatewayURL         string
	StreamTokenSecret  string
	DevBearer          string
	EnabledRegion      string
	AllowedGPUTypes    []string
	MaxActiveSandboxes int
	KubeconfigPath     string
	K8sNamespace       string
	MinWarm            int
	MaxWarm            int
	GCPProject         string
	SandboxImagePrefix string
}

func Load() (Config, error) {
	maxActive := 100
	if v := os.Getenv("IGNITION_MAX_ACTIVE_SANDBOXES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxActive = n
		}
	}
	env := strings.ToLower(strings.TrimSpace(os.Getenv("IGNITION_ENV")))
	secret := strings.TrimSpace(os.Getenv("IGNITION_STREAM_TOKEN_SECRET"))
	if secret == "" && !RequiresDatabase(env) {
		secret = defaultStreamSecret
	}
	cfg := Config{
		Env:                env,
		ListenAddr:         getenv("IGNITION_LISTEN_ADDR", ":8080"),
		DatabaseURL:        strings.TrimSpace(os.Getenv("DATABASE_URL")),
		OIDCIssuer:         strings.TrimSpace(os.Getenv("IGNITION_OIDC_ISSUER")),
		OIDCJWKSURL:        strings.TrimSpace(os.Getenv("IGNITION_OIDC_JWKS_URL")),
		OIDCAudience:       getenv("IGNITION_OIDC_AUDIENCE", "https://api.ignition.dev"),
		GatewayURL:         getenv("IGNITION_GATEWAY_URL", "https://gateway.us-central1.ignition.dev"),
		StreamTokenSecret:  secret,
		DevBearer:          strings.TrimSpace(os.Getenv("IGNITION_DEV_BEARER")),
		EnabledRegion:      getenv("IGNITION_REGION", "us-central1"),
		AllowedGPUTypes:    splitCSV(getenv("IGNITION_ALLOWED_GPU_TYPES", defaultGPUType)),
		MaxActiveSandboxes: maxActive,
		KubeconfigPath:     os.Getenv("KUBECONFIG"),
		K8sNamespace:       getenv("IGNITION_K8S_NAMESPACE", "ignition-sandboxes"),
		MinWarm:            atoiEnv("IGNITION_MIN_WARM", 1),
		MaxWarm:            atoiEnv("IGNITION_MAX_WARM", 8),
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
	if c.OIDCIssuer == "" {
		return fmt.Errorf("IGNITION_ENV=%s requires IGNITION_OIDC_ISSUER", c.Env)
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

func (c Config) GPUTypeAllowed(t string) bool {
	if len(c.AllowedGPUTypes) == 0 {
		return true
	}
	for _, allowed := range c.AllowedGPUTypes {
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
