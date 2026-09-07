// Package cli implements ignitionctl, the Ignition control-plane CLI. It is a
// thin client over the public v1 REST API: login/context, sandbox lifecycle,
// process (exec) control, and long-running operations. `exec` streams stdio
// through ignition-gateway when one is configured, else it polls.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

// Process exit codes. These are contractual: scripts depend on them.
const (
	exitOK       = 0
	exitError    = 1 // generic runtime or API error
	exitUsage    = 2 // bad invocation
	exitNotFound = 3 // 404 from the API
	exitDenied   = 4 // 403 from the API
	exitAuth     = 5 // 401 from the API
)

// exitErr carries a code so main can os.Exit precisely.
type exitErr struct {
	code int
	err  error
}

func (e *exitErr) Error() string { return e.err.Error() }
func (e *exitErr) Unwrap() error { return e.err }

func usageErrorf(format string, a ...any) error {
	return &exitErr{code: exitUsage, err: fmt.Errorf(format, a...)}
}

// ExitCode maps an error returned by Execute to a process exit code.
func ExitCode(err error) int {
	if err == nil {
		return exitOK
	}
	var ee *exitErr
	if errors.As(err, &ee) {
		return ee.code
	}
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.exitCode()
	}
	return exitError
}

// globalFlags are accepted by every subcommand, before or after positionals.
type globalFlags struct {
	server  string
	project string
	output  string // "text" (default) or "json"
	timeout int    // per-request seconds
}

func (g globalFlags) json() bool { return g.output == "json" }

type env struct {
	stdout io.Writer
	stderr io.Writer
	stdinR io.Reader
	cfg    Config
	g      globalFlags
}

func (e *env) stdin() io.Reader {
	if e.stdinR != nil {
		return e.stdinR
	}
	return os.Stdin
}

func (e *env) warnf(format string, a ...any) {
	fmt.Fprintf(e.stderr, "ignitionctl: "+format+"\n", a...)
}

// Execute runs one ignitionctl invocation and returns an error whose ExitCode
// is the process exit status.
func Execute(args []string) error {
	return run(os.Stdin, os.Stdout, os.Stderr, args)
}

func run(stdin io.Reader, stdout, stderr io.Writer, args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		printUsage(stdout)
		if len(args) == 0 {
			return &exitErr{code: exitUsage, err: errors.New("no command given")}
		}
		return nil
	}
	if args[0] == "version" {
		fmt.Fprintln(stdout, "ignitionctl (dev)")
		return nil
	}

	// Accept global flags before the command (`ignitionctl -o json sandbox
	// list`) as well as after it. A globals-only FlagSet consumes any leading
	// flags; flag.Parse stops at the first non-flag, which is the command.
	pre := newFlagSet("ignitionctl")
	preG := bindGlobals(pre)
	if err := pre.Parse(args); err != nil {
		return usageErrorf("%v", err)
	}
	rest := pre.Args()
	if len(rest) == 0 {
		printUsage(stderr)
		return &exitErr{code: exitUsage, err: errors.New("no command given")}
	}
	cmd := rest[0]
	rest = rest[1:]

	stored, err := loadConfig()
	if err != nil {
		return &exitErr{code: exitError, err: err}
	}

	dispatch := map[string]func(*env, []string) error{
		"login":     cmdLogin,
		"logout":    cmdLogout,
		"whoami":    cmdWhoami,
		"projects":  cmdProjects,
		"config":    cmdConfig,
		"sandbox":   cmdSandbox,
		"exec":      cmdExec,
		"process":   cmdProcess,
		"operation": cmdOperation,
	}
	fn, ok := dispatch[cmd]
	if !ok {
		printUsage(stderr)
		return usageErrorf("unknown command %q", cmd)
	}

	// Each command parses its own FlagSet (globals included via bindGlobals);
	// e.parse folds in only the ones explicitly set, over these pre-command
	// values.
	e := &env{stdout: stdout, stderr: stderr, stdinR: stdin, cfg: stored, g: *preG}
	return fn(e, rest)
}

// signalContext cancels on SIGINT/SIGTERM so long polls (--wait, watch) and
// exec streams exit cleanly.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `ignitionctl — Ignition control-plane CLI

Usage:
  ignitionctl <command> [flags]

Commands:
  login        Store server URL and bearer token
  logout       Remove the stored token
  whoami       Show the authenticated principal
  projects     List projects visible to the principal
  config       Show or set the local context (e.g. config set-project <id>)
  sandbox      create | list | get | terminate
  exec         Run a command in a sandbox (exec [flags] <sandbox> -- <cmd>...)
  process      list | get | signal | cancel
  operation    list | get | watch | cancel

Global flags (accepted by any command):
  --server <url>       API base URL (overrides stored / IGNITION_SERVER)
  --project <id>       Project ID (overrides stored / IGNITION_PROJECT)
  -o, --output <fmt>   text (default) or json
  --timeout <seconds>  Per-request timeout (default 30)

Environment:
  IGNITION_SERVER, IGNITION_TOKEN, IGNITION_PROJECT, IGNITION_CONFIG
`)
}
