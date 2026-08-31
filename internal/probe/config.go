package probe

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the ignition-prober process configuration. It has its own env
// namespace and is not part of internal/config.Config.
type Config struct {
	Target   string        // base URL of the API, e.g. http://ignition-api.ignition-system:8080
	Project  string        // project id the journeys run against
	ImageID  string        // seeded image id for sandbox journeys
	Audience string        // OIDC audience for the minted ID token
	Auth     string        // "gcp-idtoken" | "static" | "none"
	Token    string        // static bearer (Auth == "static")
	Journeys string        // "full" | "lite" | comma list
	Interval time.Duration // between continuous cycles
	Timeout  time.Duration // per-cycle deadline
	Listen   string        // metrics/health listen address
	OneShot  bool          // run once and exit non-zero on failure
}

// Load reads IGNITION_PROBE_* environment variables.
func Load() (Config, error) {
	c := Config{
		Target:   os.Getenv("IGNITION_PROBE_TARGET"),
		Project:  getenv("IGNITION_PROBE_PROJECT", "prj_dev"),
		ImageID:  getenv("IGNITION_PROBE_IMAGE", "img_seed"),
		Audience: os.Getenv("IGNITION_PROBE_AUDIENCE"),
		Auth:     getenv("IGNITION_PROBE_AUTH", "gcp-idtoken"),
		Token:    os.Getenv("IGNITION_PROBE_TOKEN"),
		Journeys: getenv("IGNITION_PROBE_JOURNEYS", "full"),
		Interval: durEnv("IGNITION_PROBE_INTERVAL", 5*time.Minute),
		Timeout:  durEnv("IGNITION_PROBE_TIMEOUT", 10*time.Minute),
		Listen:   getenv("IGNITION_PROBE_LISTEN", ":9102"),
		OneShot:  boolEnv("IGNITION_PROBE_ONESHOT"),
	}
	if c.Target == "" {
		return c, fmt.Errorf("IGNITION_PROBE_TARGET is required")
	}
	c.Target = strings.TrimRight(c.Target, "/")
	if c.Audience == "" {
		c.Audience = c.Target
	}
	switch c.Auth {
	case "gcp-idtoken", "static", "none":
	default:
		return c, fmt.Errorf("IGNITION_PROBE_AUTH must be gcp-idtoken|static|none, got %q", c.Auth)
	}
	if c.Auth == "static" && c.Token == "" {
		return c, fmt.Errorf("IGNITION_PROBE_AUTH=static requires IGNITION_PROBE_TOKEN")
	}
	if _, err := Select(c.Journeys); err != nil {
		return c, err
	}
	return c, nil
}

func getenv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func durEnv(key string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

func boolEnv(key string) bool {
	b, _ := strconv.ParseBool(strings.TrimSpace(os.Getenv(key)))
	return b
}
