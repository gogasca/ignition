package api

import (
	"fmt"
	"net"
	"regexp"
	"strings"
	"unicode"

	"ignition.dev/ignition/internal/store"
)

const (
	maxCPUMilli       = 8000
	maxMemoryMiB      = 32768
	maxStartupSeconds = 600
	maxRuntimeSeconds = 86400
	maxIdleSeconds    = 3600
	maxGraceSeconds   = 120
	maxCommandArgs    = 32
	maxCommandBytes   = 16 << 10
	maxEnvKeys        = 32
	maxLabels         = 32
	maxSecretRefs     = 16
	maxIdempotencyKey = 128
	maxRequestIDLen   = 128
)

var allowedSignals = map[string]struct{}{
	"SIGTERM": {},
	"SIGINT":  {},
	"SIGKILL": {},
	"SIGHUP":  {},
	"SIGQUIT": {},
	"SIGUSR1": {},
	"SIGUSR2": {},
}

func validSignal(sig string) bool {
	_, ok := allowedSignals[sig]
	return ok
}

func checkCommand(cmd []string) error {
	if len(cmd) > maxCommandArgs {
		return fmt.Errorf("command has too many arguments")
	}
	n := 0
	for _, a := range cmd {
		n += len(a)
		if n > maxCommandBytes {
			return fmt.Errorf("command is too large")
		}
	}
	return nil
}

func checkEnv(env map[string]string) error {
	if len(env) > maxEnvKeys {
		return fmt.Errorf("too many environment variables")
	}
	return nil
}

func checkLabels(labels map[string]string) error {
	if len(labels) > maxLabels {
		return fmt.Errorf("too many labels")
	}
	return nil
}

var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func checkSecretRefs(refs []store.SecretRef, env map[string]string) error {
	if len(refs) > maxSecretRefs {
		return fmt.Errorf("too many secretRefs")
	}
	seen := map[string]struct{}{}
	for _, ref := range refs {
		if ref.SecretID == "" || !store.ValidImageID(ref.SecretID) {
			return fmt.Errorf("secretRefs.secretId is invalid")
		}
		if ref.EnvironmentName == "" || !envNameRe.MatchString(ref.EnvironmentName) {
			return fmt.Errorf("secretRefs.environmentName is invalid")
		}
		if strings.HasPrefix(ref.EnvironmentName, "IGNITION_") {
			return fmt.Errorf("secretRefs.environmentName %q is reserved", ref.EnvironmentName)
		}
		if _, ok := seen[ref.EnvironmentName]; ok {
			return fmt.Errorf("secretRefs.environmentName %q is duplicated", ref.EnvironmentName)
		}
		seen[ref.EnvironmentName] = struct{}{}
		if _, ok := env[ref.EnvironmentName]; ok {
			return fmt.Errorf("secretRefs.environmentName %q collides with environment", ref.EnvironmentName)
		}
	}
	return nil
}

func checkTimeouts(t store.TimeoutSpec) error {
	if t.StartupSeconds < 1 || t.StartupSeconds > maxStartupSeconds {
		return fmt.Errorf("timeouts.startupSeconds must be between 1 and %d", maxStartupSeconds)
	}
	if t.MaximumRuntimeSeconds < 1 || t.MaximumRuntimeSeconds > maxRuntimeSeconds {
		return fmt.Errorf("timeouts.maximumRuntimeSeconds must be between 1 and %d", maxRuntimeSeconds)
	}
	if t.IdleSeconds < 1 || t.IdleSeconds > maxIdleSeconds {
		return fmt.Errorf("timeouts.idleSeconds must be between 1 and %d", maxIdleSeconds)
	}
	if t.TerminationGraceSeconds < 1 || t.TerminationGraceSeconds > maxGraceSeconds {
		return fmt.Errorf("timeouts.terminationGraceSeconds must be between 1 and %d", maxGraceSeconds)
	}
	return nil
}

// blockedEgressCIDRs are networks an ALLOW_LIST egress rule may never reach:
// "this host", private (RFC 1918 / ULA), loopback, link-local (including the
// 169.254.169.254 metadata endpoint), carrier-grade NAT, 6to4 and NAT64 space
// (which embed an arbitrary inner address), multicast, and the
// reserved/documentation/benchmark ranges. A rule is rejected when its range
// overlaps any of these, so a short prefix such as "8.8.8.8/1" cannot smuggle
// private space past the check. IPv4-mapped IPv6 ("::ffff:a.b.c.d") is
// normalized to IPv4 by net.ParseCIDR, so the IPv4 entries cover it.
var blockedEgressCIDRs = mustParseCIDRs(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
	"169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
	"192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24",
	"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
	"::/128", "::1/128", "64:ff9b::/96", "100::/64",
	"2001:db8::/32", "2002::/16", "fc00::/7", "fe80::/10", "ff00::/8",
)

func mustParseCIDRs(cidrs ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic("limits: invalid blocked CIDR " + c)
		}
		out = append(out, n)
	}
	return out
}

// cidrsOverlap reports whether two CIDR blocks intersect. Two CIDR ranges are
// always either disjoint or nested, so a mutual Contains check on the network
// addresses is sufficient.
func cidrsOverlap(a, b *net.IPNet) bool {
	return a.Contains(b.IP) || b.Contains(a.IP)
}

func checkAllowList(e store.EgressSpec) error {
	if len(e.AllowedTLSDomains) > 0 {
		return fmt.Errorf("allowedTlsDomains is unavailable until the egress proxy is configured")
	}
	if len(e.AllowedTLSDomains) == 0 && len(e.AllowedCIDRs) == 0 {
		return fmt.Errorf("ALLOW_LIST requires allowedTlsDomains or allowedCidrs")
	}
	for _, cidr := range e.AllowedCIDRs {
		_, ipnet, err := net.ParseCIDR(cidr)
		if err != nil {
			return fmt.Errorf("invalid allowedCidr %q", cidr)
		}
		for _, blocked := range blockedEgressCIDRs {
			if cidrsOverlap(ipnet, blocked) {
				return fmt.Errorf("allowedCidr %q overlaps blocked network %s", cidr, blocked)
			}
		}
	}
	return nil
}

func sanitizeRequestID(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
			if b.Len() >= maxRequestIDLen {
				break
			}
		}
	}
	return b.String()
}
