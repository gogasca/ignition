package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"ignition.dev/ignition/internal/store"
)

const (
	defaultStreamSecret = "dev-stream-token-secret"
	// Both accelerator classes are allowed by default so the built-in CPU
	// default runtime is usable without extra configuration.
	defaultAccelerators = "NONE,NVIDIA_L4"

	// GoogleOIDCIssuer is the issuer for Google-minted ID tokens (service
	// accounts via Workload Identity / impersonation, and users). When
	// IGNITION_OIDC_ISSUER is this value the loader defaults the subject
	// claim to `email` and the accepted token type to `JWT`.
	GoogleOIDCIssuer = "https://accounts.google.com"
	// Defaults for the Cloud IAP assertion verifier.
	defaultIAPIssuer  = "https://cloud.google.com/iap"
	defaultIAPJWKSURL = "https://www.gstatic.com/iap/verify/public_key-jwk"
)

// Config is process configuration shared by control-plane binaries.
type Config struct {
	Env                 string
	ListenAddr          string
	AdminAddr           string
	DatabaseURL         string
	OIDCIssuer          string
	OIDCJWKSURL         string
	OIDCAudience        string
	OIDCAudiences       []string
	OIDCSubjectClaim    string
	OIDCHostedDomains   []string
	OIDCAllowedTypes    []string
	IAPEnabled          bool
	IAPIssuer           string
	IAPAudience         string
	IAPJWKSURL          string
	BootstrapProject    string
	BootstrapAdmin      string
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
	MinWarmCPU          int
	MaxWarmCPU          int
	WarmWindow          time.Duration
	NodeProvisionTime   time.Duration
	GCPProject          string
	SandboxImagePrefix  string
	// DefaultRuntime fills any RuntimeSpec field a CreateSandbox request
	// leaves unset. Overridden by IGNITION_DEFAULT_RUNTIME (JSON); the
	// built-in fallback is a CPU-only sandbox.
	DefaultRuntime store.RuntimeSpec
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
	// CPU warm capacity defaults to disabled (0/0): the CPU sandbox pool has
	// historically scaled to zero, and turning on a buffer has a real node
	// cost, so it stays opt-in rather than changing existing deployments'
	// spend on upgrade. An operator sets both to enable it.
	maxWarmCPU := atoiEnv("IGNITION_MAX_WARM_CPU", 0)
	minWarmCPU := atoiEnv("IGNITION_MIN_WARM_CPU", min(1, maxWarmCPU))
	warmWindowSeconds := atoiEnv("IGNITION_WARM_WINDOW_SECONDS", 15*60)
	if warmWindowSeconds <= 0 {
		return Config{}, fmt.Errorf("IGNITION_WARM_WINDOW_SECONDS must be greater than zero")
	}
	nodeProvisionSeconds := atoiEnv("IGNITION_NODE_PROVISION_SECONDS", 4*60)
	if nodeProvisionSeconds <= 0 {
		return Config{}, fmt.Errorf("IGNITION_NODE_PROVISION_SECONDS must be greater than zero")
	}
	warmWindow := time.Duration(warmWindowSeconds) * time.Second
	nodeProvisionTime := time.Duration(nodeProvisionSeconds) * time.Second
	secret := strings.TrimSpace(os.Getenv("IGNITION_STREAM_TOKEN_SECRET"))
	if secret == "" && !RequiresDatabase(env) {
		secret = defaultStreamSecret
	}
	oidcIssuer := strings.TrimSpace(os.Getenv("IGNITION_OIDC_ISSUER"))
	google := oidcIssuer == GoogleOIDCIssuer

	subjectClaim := strings.TrimSpace(os.Getenv("IGNITION_OIDC_SUBJECT_CLAIM"))
	if subjectClaim == "" && google {
		subjectClaim = "email"
	}
	allowedTypes := splitCSV(os.Getenv("IGNITION_OIDC_ALLOWED_TYPES"))
	if len(allowedTypes) == 0 && google {
		allowedTypes = []string{"JWT"}
	}

	cfg := Config{
		Env:               env,
		ListenAddr:        getenv("IGNITION_LISTEN_ADDR", ":8080"),
		AdminAddr:         getenv("IGNITION_ADMIN_ADDR", ":9090"),
		DatabaseURL:       strings.TrimSpace(os.Getenv("DATABASE_URL")),
		OIDCIssuer:        oidcIssuer,
		OIDCJWKSURL:       strings.TrimSpace(os.Getenv("IGNITION_OIDC_JWKS_URL")),
		OIDCAudience:      getenv("IGNITION_OIDC_AUDIENCE", "https://api.ignition.dev"),
		OIDCAudiences:     splitCSV(os.Getenv("IGNITION_OIDC_AUDIENCES")),
		OIDCSubjectClaim:  subjectClaim,
		OIDCHostedDomains: splitCSV(os.Getenv("IGNITION_OIDC_HOSTED_DOMAINS")),
		OIDCAllowedTypes:  allowedTypes,
		IAPEnabled:        boolEnv("IGNITION_IAP_ENABLED"),
		IAPIssuer:         getenv("IGNITION_IAP_ISSUER", defaultIAPIssuer),
		IAPAudience:       strings.TrimSpace(os.Getenv("IGNITION_IAP_AUDIENCE")),
		IAPJWKSURL:        getenv("IGNITION_IAP_JWKS_URL", defaultIAPJWKSURL),
		BootstrapProject:  strings.TrimSpace(os.Getenv("IGNITION_BOOTSTRAP_PROJECT")),
		BootstrapAdmin:    strings.ToLower(strings.TrimSpace(os.Getenv("IGNITION_BOOTSTRAP_ADMIN"))),
		GatewayURL:        getenv("IGNITION_GATEWAY_URL", "https://gateway.us-central1.ignition.dev"),
		StreamTokenSecret: secret,
		DevBearer:         strings.TrimSpace(os.Getenv("IGNITION_DEV_BEARER")),
		EnabledRegion:     getenv("IGNITION_REGION", "us-central1"),
		AllowedAccelerators: splitCSV(getenv("IGNITION_ALLOWED_ACCELERATORS",
			getenv("IGNITION_ALLOWED_GPU_TYPES", defaultAccelerators))),
		MaxActiveSandboxes: maxActive,
		KubeconfigPath:     os.Getenv("KUBECONFIG"),
		K8sNamespace:       getenv("IGNITION_K8S_NAMESPACE", "ignition-sandboxes"),
		MinWarm:            minWarm,
		MaxWarm:            maxWarm,
		MinWarmCPU:         minWarmCPU,
		MaxWarmCPU:         maxWarmCPU,
		WarmWindow:         warmWindow,
		NodeProvisionTime:  nodeProvisionTime,
		GCPProject:         strings.TrimSpace(os.Getenv("IGNITION_GCP_PROJECT")),
		SandboxImagePrefix: strings.TrimSpace(os.Getenv("IGNITION_SANDBOX_IMAGE_PREFIX")),
	}
	rt, err := parseDefaultRuntime(os.Getenv("IGNITION_DEFAULT_RUNTIME"))
	if err != nil {
		return Config{}, err
	}
	cfg.DefaultRuntime = rt
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// parseDefaultRuntime merges an optional IGNITION_DEFAULT_RUNTIME JSON object
// over the built-in CPU default, so an operator can override just the fields
// they care about.
func parseDefaultRuntime(raw string) (store.RuntimeSpec, error) {
	base := store.BuiltinDefaultRuntime()
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return base, nil
	}
	var override store.RuntimeSpec
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&override); err != nil {
		return store.RuntimeSpec{}, fmt.Errorf("IGNITION_DEFAULT_RUNTIME is not a valid RuntimeSpec: %w", err)
	}
	return store.MergeRuntime(base, override), nil
}

// Validate fails closed for staging/prod misconfiguration.
func (c Config) Validate() error {
	if c.MinWarm < 0 || c.MaxWarm < 0 || c.MinWarm > c.MaxWarm {
		return fmt.Errorf("IGNITION_MIN_WARM (%d) and IGNITION_MAX_WARM (%d) must satisfy 0 <= min <= max", c.MinWarm, c.MaxWarm)
	}
	if c.MinWarmCPU < 0 || c.MaxWarmCPU < 0 || c.MinWarmCPU > c.MaxWarmCPU {
		return fmt.Errorf("IGNITION_MIN_WARM_CPU (%d) and IGNITION_MAX_WARM_CPU (%d) must satisfy 0 <= min <= max", c.MinWarmCPU, c.MaxWarmCPU)
	}
	if c.WarmWindow < 0 {
		return fmt.Errorf("warm window must not be negative")
	}
	if c.NodeProvisionTime < 0 {
		return fmt.Errorf("node provision time must not be negative")
	}
	rt := c.EffectiveDefaultRuntime()
	if err := store.ValidateRuntimeSpec(rt); err != nil {
		return fmt.Errorf("IGNITION_DEFAULT_RUNTIME: %w", err)
	}
	if !c.AcceleratorAllowed(rt.Resources.Accelerator.Type) {
		return fmt.Errorf("default runtime accelerator %q is not permitted by IGNITION_ALLOWED_ACCELERATORS", rt.Resources.Accelerator.Type)
	}
	switch c.OIDCSubjectClaim {
	case "", "sub", "email":
	default:
		return fmt.Errorf("IGNITION_OIDC_SUBJECT_CLAIM must be empty, \"sub\", or \"email\"")
	}
	if c.IAPEnabled && strings.TrimSpace(c.IAPAudience) == "" {
		return fmt.Errorf("IGNITION_IAP_ENABLED requires IGNITION_IAP_AUDIENCE")
	}
	if (c.BootstrapProject == "") != (c.BootstrapAdmin == "") {
		return fmt.Errorf("IGNITION_BOOTSTRAP_PROJECT and IGNITION_BOOTSTRAP_ADMIN must be set together")
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
	httpsURLs := map[string]string{
		"IGNITION_OIDC_ISSUER":   c.OIDCIssuer,
		"IGNITION_OIDC_JWKS_URL": c.OIDCJWKSURL,
		"IGNITION_GATEWAY_URL":   c.GatewayURL,
	}
	if c.IAPEnabled {
		httpsURLs["IGNITION_IAP_ISSUER"] = c.IAPIssuer
		httpsURLs["IGNITION_IAP_JWKS_URL"] = c.IAPJWKSURL
	}
	for name, raw := range httpsURLs {
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

// EffectiveDefaultRuntime returns DefaultRuntime, or the built-in CPU default
// when it is unset (hand-built Config values in tests).
func (c Config) EffectiveDefaultRuntime() store.RuntimeSpec {
	if c.DefaultRuntime == (store.RuntimeSpec{}) {
		return store.BuiltinDefaultRuntime()
	}
	return c.DefaultRuntime
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

func boolEnv(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
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
