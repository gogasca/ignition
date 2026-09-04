package api

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"ignition.dev/ignition/internal/store"
)

// RuntimeSpec caps (cpu/memory/timeouts) live in internal/store as exported
// constants so the default runtime is validated the same way at startup.
const (
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

func checkSecretRefs(refs []store.SecretRef) error {
	if len(refs) > maxSecretRefs {
		return fmt.Errorf("too many secretRefs")
	}
	seen := map[string]struct{}{}
	for _, ref := range refs {
		if ref.SecretID == "" || !store.ValidImageID(ref.SecretID) {
			return fmt.Errorf("secretRefs.secretId is invalid")
		}
		if !store.ValidSecretVersion(ref.Version) {
			return fmt.Errorf("secretRefs.version is invalid")
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
