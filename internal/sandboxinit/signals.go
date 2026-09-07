package sandboxinit

import "syscall"

// allowedSignals matches the API's allowlist (internal/api/limits.go).
var allowedSignals = map[string]syscall.Signal{
	"SIGTERM": syscall.SIGTERM,
	"SIGINT":  syscall.SIGINT,
	"SIGKILL": syscall.SIGKILL,
	"SIGHUP":  syscall.SIGHUP,
	"SIGQUIT": syscall.SIGQUIT,
	"SIGUSR1": syscall.SIGUSR1,
	"SIGUSR2": syscall.SIGUSR2,
}

func unixSignal(name string) syscall.Signal {
	return allowedSignals[name] // 0 (invalid) when not allowlisted
}

func signalName(sig syscall.Signal) string {
	for name, s := range allowedSignals {
		if s == sig {
			return name
		}
	}
	return sig.String()
}
