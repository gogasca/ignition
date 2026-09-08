package cli

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type processJSON struct {
	ID                string            `json:"id"`
	ProjectID         string            `json:"projectId"`
	SandboxID         string            `json:"sandboxId"`
	State             string            `json:"state"`
	Command           []string          `json:"command"`
	WorkingDirectory  string            `json:"workingDirectory"`
	Environment       map[string]string `json:"environment"`
	PTY               bool              `json:"pty"`
	CreateTime        string            `json:"createTime"`
	StartTime         string            `json:"startTime"`
	ExitTime          string            `json:"exitTime"`
	ExitCode          *int              `json:"exitCode"`
	TerminatingSignal string            `json:"terminatingSignal"`
}

type attachJSON struct {
	StreamToken string `json:"streamToken"`
	GatewayURL  string `json:"gatewayUrl"`
	ExpireTime  string `json:"expireTime"`
	StreamEpoch int64  `json:"streamEpoch"`
}

// cmdExec: create a process in a sandbox and follow it to completion.
// Byte streaming (stdin/stdout) arrives with ignition-gateway; until then this
// reports process lifecycle and propagates the exit code.
func cmdExec(e *env, args []string) error {
	fs := newFlagSet("exec")
	g := bindGlobals(fs)
	var (
		pty      bool
		workdir  string
		noWait   bool
		showTok  bool
		noStream bool
		deadline int
		envs     kvSlice
	)
	fs.BoolVar(&pty, "tty", false, "request a PTY")
	fs.StringVar(&workdir, "workdir", "", "working directory inside the sandbox")
	fs.BoolVar(&noWait, "no-wait", false, "return after creating the process")
	fs.BoolVar(&showTok, "attach-token", false, "also print a gateway attach token")
	fs.BoolVar(&noStream, "no-stream", false, "poll process state instead of streaming stdio via the gateway")
	fs.IntVar(&deadline, "deadline", 600, "seconds to follow the process before giving up")
	fs.Var(&envs, "env", "environment variable KEY=VALUE (repeatable)")
	// exec does not use interspersed parsing: flags stop at the sandbox ID and
	// everything after it (past an optional "--") is the guest command verbatim,
	// so guest flags like `ls -l` are never interpreted by ignitionctl.
	if err := fs.Parse(args); err != nil {
		return usageErrorf("%v", err)
	}
	if err := e.foldGlobals(fs, g); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) < 2 {
		return usageErrorf("usage: ignitionctl exec [flags] <sandbox> -- <command> [args...]")
	}
	sandboxID := rest[0]
	command := rest[1:]
	if command[0] == "--" {
		command = command[1:]
	}
	if len(command) == 0 {
		return usageErrorf("a command is required after the sandbox")
	}

	c, err := e.projectClient()
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()

	body := map[string]any{"command": command, "pty": pty}
	if workdir != "" {
		body["workingDirectory"] = workdir
	}
	if len(envs) > 0 {
		body["environment"] = map[string]string(envs)
	}
	var proc processJSON
	if err := c.do(ctx, "POST", c.processesPath(sandboxID), body, &proc, requestOptions{idempotent: true}); err != nil {
		return err
	}

	if showTok {
		var at attachJSON
		if err := c.do(ctx, "POST", c.processPath(sandboxID, proc.ID)+":attach", map[string]any{}, &at, requestOptions{idempotent: true}); err != nil {
			return err
		}
		if e.g.json() {
			e.emit(map[string]any{"process": proc, "attach": at})
		} else {
			e.printf("process:     %s\n", proc.ID)
			e.printf("gatewayUrl:  %s\n", at.GatewayURL)
			e.printf("streamToken: %s\n", at.StreamToken)
			e.printf("expireTime:  %s\n", at.ExpireTime)
		}
		return nil
	}

	if noWait {
		if e.emit(proc) {
			return nil
		}
		e.printf("%s %s\n", proc.ID, proc.State)
		return nil
	}

	// Preferred path: stream stdio through ignition-gateway. Falls back to
	// polling when the deployment has no gateway or --no-stream is set.
	if !noStream && !e.g.json() {
		var at attachJSON
		aerr := c.do(ctx, "POST", c.processPath(sandboxID, proc.ID)+":attach", map[string]any{}, &at, requestOptions{idempotent: true})
		if aerr == nil && at.GatewayURL != "" && at.StreamToken != "" {
			code, serr := streamExec(ctx, at.GatewayURL, at.StreamToken, proc.ID, e.stdin(), e.stdout, e.stderr)
			if serr == nil {
				if code > 0 {
					return &exitErr{code: code, err: fmt.Errorf("process exited with code %d", code)}
				}
				return nil
			}
			e.warnf("stream unavailable (%v); falling back to polling", serr)
		}
	}

	final, werr := followProcess(ctx, c, sandboxID, proc.ID, time.Duration(deadline)*time.Second)
	if final.ID != "" {
		proc = final
	}
	if e.emit(proc) {
		// still set the exit code below
	} else {
		renderProcess(e, proc)
	}
	if werr != nil {
		return werr
	}
	if proc.State == "EXITED" && proc.ExitCode != nil && *proc.ExitCode != 0 {
		return &exitErr{code: *proc.ExitCode, err: fmt.Errorf("process exited with code %d", *proc.ExitCode)}
	}
	if proc.State == "FAILED" {
		return &exitErr{code: exitError, err: fmt.Errorf("process %s FAILED", proc.ID)}
	}
	return nil
}

func followProcess(ctx context.Context, c *client, sandboxID, processID string, timeout time.Duration) (processJSON, error) {
	deadline := time.Now().Add(timeout)
	var last processJSON
	for {
		var p processJSON
		if err := c.do(ctx, "GET", c.processPath(sandboxID, processID), nil, &p, requestOptions{}); err != nil {
			return last, err
		}
		last = p
		if p.State == "EXITED" || p.State == "FAILED" {
			return last, nil
		}
		if time.Now().After(deadline) {
			return last, fmt.Errorf("timed out after %s following process %s (state %s)", timeout, processID, p.State)
		}
		if !sleep(ctx) {
			return last, errors.New("interrupted")
		}
	}
}

func cmdProcess(e *env, args []string) error {
	if len(args) == 0 {
		return usageErrorf("usage: ignitionctl process <list|get|signal|cancel>")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list", "ls":
		return processList(e, rest)
	case "get", "describe":
		return processGet(e, rest)
	case "signal":
		return processSignal(e, rest)
	case "cancel":
		return processCancel(e, rest)
	default:
		return usageErrorf("unknown process subcommand %q", sub)
	}
}

func processList(e *env, args []string) error {
	fs := newFlagSet("process list")
	g := bindGlobals(fs)
	pos, err := e.parse(fs, g, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErrorf("usage: ignitionctl process list <sandbox>")
	}
	c, err := e.projectClient()
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	var resp struct {
		Processes     []processJSON `json:"processes"`
		NextPageToken string        `json:"nextPageToken"`
	}
	if err := c.do(ctx, "GET", c.processesPath(pos[0]), nil, &resp, requestOptions{}); err != nil {
		return err
	}
	if e.emit(resp) {
		return nil
	}
	if len(resp.Processes) == 0 {
		e.printf("no processes\n")
		return nil
	}
	rows := make([][]string, 0, len(resp.Processes))
	for _, p := range resp.Processes {
		rows = append(rows, []string{p.ID, p.State, exitLabel(p), argv0(p.Command), ageOf(p.CreateTime)})
	}
	table(e.stdout, []string{"ID", "STATE", "EXIT", "COMMAND", "AGE"}, rows)
	return nil
}

func processGet(e *env, args []string) error {
	fs := newFlagSet("process get")
	g := bindGlobals(fs)
	pos, err := e.parse(fs, g, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return usageErrorf("usage: ignitionctl process get <sandbox> <process>")
	}
	c, err := e.projectClient()
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	var p processJSON
	if err := c.do(ctx, "GET", c.processPath(pos[0], pos[1]), nil, &p, requestOptions{}); err != nil {
		return err
	}
	if e.emit(p) {
		return nil
	}
	renderProcess(e, p)
	return nil
}

func processSignal(e *env, args []string) error {
	fs := newFlagSet("process signal")
	g := bindGlobals(fs)
	var sig string
	fs.StringVar(&sig, "signal", "SIGTERM", "signal name (SIGTERM, SIGINT, SIGKILL, SIGHUP, SIGQUIT, SIGUSR1, SIGUSR2)")
	pos, err := e.parse(fs, g, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return usageErrorf("usage: ignitionctl process signal <sandbox> <process> [--signal SIGTERM]")
	}
	c, err := e.projectClient()
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	var p processJSON
	if err := c.do(ctx, "POST", c.processPath(pos[0], pos[1])+":signal", map[string]any{"signal": sig}, &p, requestOptions{idempotent: true}); err != nil {
		return err
	}
	if e.emit(p) {
		return nil
	}
	e.printf("%s %s\n", p.ID, p.State)
	return nil
}

func processCancel(e *env, args []string) error {
	fs := newFlagSet("process cancel")
	g := bindGlobals(fs)
	pos, err := e.parse(fs, g, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return usageErrorf("usage: ignitionctl process cancel <sandbox> <process>")
	}
	c, err := e.projectClient()
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	var p processJSON
	if err := c.do(ctx, "POST", c.processPath(pos[0], pos[1])+":cancel", map[string]any{}, &p, requestOptions{idempotent: true}); err != nil {
		return err
	}
	if e.emit(p) {
		return nil
	}
	e.printf("%s %s\n", p.ID, p.State)
	return nil
}

func renderProcess(e *env, p processJSON) {
	e.printf("ID:        %s\n", p.ID)
	e.printf("Sandbox:   %s\n", p.SandboxID)
	e.printf("State:     %s\n", p.State)
	e.printf("Command:   %s\n", shellJoin(p.Command))
	if p.WorkingDirectory != "" {
		e.printf("Workdir:   %s\n", p.WorkingDirectory)
	}
	e.printf("PTY:       %t\n", p.PTY)
	e.printf("Created:   %s\n", orDash(p.CreateTime))
	if p.StartTime != "" {
		e.printf("Started:   %s\n", p.StartTime)
	}
	if p.ExitTime != "" {
		e.printf("Exited:    %s\n", p.ExitTime)
	}
	if p.ExitCode != nil {
		e.printf("ExitCode:  %d\n", *p.ExitCode)
	}
	if p.TerminatingSignal != "" {
		e.printf("Signal:    %s\n", p.TerminatingSignal)
	}
}

func exitLabel(p processJSON) string {
	if p.ExitCode != nil {
		return fmt.Sprint(*p.ExitCode)
	}
	if p.TerminatingSignal != "" {
		return p.TerminatingSignal
	}
	return "-"
}

func argv0(cmd []string) string {
	if len(cmd) == 0 {
		return "-"
	}
	return cmd[0]
}

func shellJoin(cmd []string) string {
	if len(cmd) == 0 {
		return "-"
	}
	out := ""
	for i, a := range cmd {
		if i > 0 {
			out += " "
		}
		out += a
	}
	return out
}
