package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

type sandboxJSON struct {
	ID          string            `json:"id"`
	ProjectID   string            `json:"projectId"`
	Name        string            `json:"name"`
	State       string            `json:"state"`
	StateReason string            `json:"stateReason"`
	ImageID     string            `json:"imageId"`
	OperationID string            `json:"operationId"`
	CreateTime  string            `json:"createTime"`
	ReadyTime   string            `json:"readyTime"`
	FinishTime  string            `json:"finishTime"`
	Labels      map[string]string `json:"labels"`
	Resources   struct {
		CPUMilli    int `json:"cpuMilli"`
		MemoryMiB   int `json:"memoryMiB"`
		Accelerator struct {
			Count int    `json:"count"`
			Type  string `json:"type"`
		} `json:"accelerator"`
	} `json:"resources"`
}

func cmdSandbox(e *env, args []string) error {
	if len(args) == 0 {
		return usageErrorf("usage: ignitionctl sandbox <create|list|get|terminate>")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "create":
		return sandboxCreate(e, rest)
	case "list", "ls":
		return sandboxList(e, rest)
	case "get", "describe":
		return sandboxGet(e, rest)
	case "terminate", "delete", "rm":
		return sandboxTerminate(e, rest)
	default:
		return usageErrorf("unknown sandbox subcommand %q", sub)
	}
}

func sandboxCreate(e *env, args []string) error {
	fs := newFlagSet("sandbox create")
	g := bindGlobals(fs)
	var (
		image       string
		name        string
		cpu         int
		mem         int
		accelerator string
		count       int
		internet    bool
		wait        bool
		waitTimeout int
		envs        kvSlice
		labels      kvSlice
		cmdArgs     strSlice
	)
	fs.StringVar(&image, "image", "", "image ID (required)")
	fs.StringVar(&name, "name", "", "friendly name")
	fs.IntVar(&cpu, "cpu", 1000, "CPU in millicores")
	fs.IntVar(&mem, "memory", 2048, "memory in MiB")
	fs.StringVar(&accelerator, "accelerator", "NVIDIA_L4", "accelerator type (NVIDIA_L4 or NONE)")
	fs.IntVar(&count, "count", -1, "accelerator count (default: 1 for GPU, 0 for NONE)")
	fs.BoolVar(&internet, "internet", false, "allow outbound internet")
	fs.BoolVar(&wait, "wait", false, "wait until READY (or terminal)")
	fs.IntVar(&waitTimeout, "wait-timeout", 300, "seconds to wait when --wait is set")
	fs.Var(&envs, "env", "environment variable KEY=VALUE (repeatable)")
	fs.Var(&labels, "label", "label KEY=VALUE (repeatable)")
	fs.Var(&cmdArgs, "command", "command argument (repeatable); or pass after --")
	pos, err := e.parse(fs, g, args)
	if err != nil {
		return err
	}
	command := append([]string(cmdArgs), pos...)
	if image == "" {
		return usageErrorf("--image is required")
	}
	if count < 0 {
		if accelerator == "NONE" {
			count = 0
		} else {
			count = 1
		}
	}
	_ = envs // env is not a create-sandbox field today; reserved for parity with exec

	internetMode := "DISABLED"
	if internet {
		internetMode = "ENABLED"
	}
	body := map[string]any{
		"imageId": image,
		"resources": map[string]any{
			"cpuMilli":    cpu,
			"memoryMiB":   mem,
			"accelerator": map[string]any{"type": accelerator, "count": count},
		},
		"network": map[string]any{"internetAccess": internetMode},
	}
	if name != "" {
		body["name"] = name
	}
	if len(command) > 0 {
		body["command"] = command
	}
	if len(labels) > 0 {
		body["labels"] = map[string]string(labels)
	}

	c, err := e.client()
	if err != nil {
		return err
	}
	if _, err := e.requireProject(); err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()

	var created struct {
		Sandbox   sandboxJSON    `json:"sandbox"`
		Operation map[string]any `json:"operation"`
	}
	if err := c.do(ctx, "POST", sandboxesPath(), body, &created, requestOptions{idempotent: true}); err != nil {
		return err
	}
	sb := created.Sandbox

	if wait {
		final, werr := waitSandbox(ctx, c, sb.ID, time.Duration(waitTimeout)*time.Second)
		if final.ID != "" {
			sb = final
		}
		if werr != nil {
			renderSandbox(e, sb)
			return werr
		}
	}

	if e.emit(map[string]any{"sandbox": sb, "operation": created.Operation}) {
		return nil
	}
	renderSandbox(e, sb)
	return nil
}

func sandboxList(e *env, args []string) error {
	fs := newFlagSet("sandbox list")
	g := bindGlobals(fs)
	var pageSize int
	fs.IntVar(&pageSize, "limit", 0, "max rows (server default when 0)")
	if _, err := e.parse(fs, g, args); err != nil {
		return err
	}
	c, err := e.client()
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()

	q := map[string]string{}
	if pageSize > 0 {
		q["pageSize"] = fmt.Sprint(pageSize)
	}
	var resp struct {
		Sandboxes     []sandboxJSON `json:"sandboxes"`
		NextPageToken string        `json:"nextPageToken"`
	}
	if err := c.do(ctx, "GET", sandboxesPath(), nil, &resp, requestOptions{query: q}); err != nil {
		return err
	}
	if e.emit(resp) {
		return nil
	}
	if len(resp.Sandboxes) == 0 {
		e.printf("no sandboxes\n")
		return nil
	}
	rows := make([][]string, 0, len(resp.Sandboxes))
	for _, s := range resp.Sandboxes {
		rows = append(rows, []string{s.ID, s.State, s.ImageID, acceleratorLabel(s), ageOf(s.CreateTime)})
	}
	table(e.stdout, []string{"ID", "STATE", "IMAGE", "ACCEL", "AGE"}, rows)
	if resp.NextPageToken != "" {
		e.printf("\n(more results; pass --limit or use -o json for the page token)\n")
	}
	return nil
}

func sandboxGet(e *env, args []string) error {
	fs := newFlagSet("sandbox get")
	g := bindGlobals(fs)
	pos, err := e.parse(fs, g, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErrorf("usage: ignitionctl sandbox get <sandbox>")
	}
	c, err := e.client()
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	var sb sandboxJSON
	if err := c.do(ctx, "GET", sandboxPath(pos[0]), nil, &sb, requestOptions{}); err != nil {
		return err
	}
	if e.emit(sb) {
		return nil
	}
	renderSandbox(e, sb)
	return nil
}

func sandboxTerminate(e *env, args []string) error {
	fs := newFlagSet("sandbox terminate")
	g := bindGlobals(fs)
	var wait bool
	var waitTimeout int
	fs.BoolVar(&wait, "wait", false, "wait until the sandbox is FINISHED/FAILED")
	fs.IntVar(&waitTimeout, "wait-timeout", 120, "seconds to wait when --wait is set")
	pos, err := e.parse(fs, g, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErrorf("usage: ignitionctl sandbox terminate <sandbox>")
	}
	id := pos[0]
	c, err := e.client()
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()

	var resp struct {
		Sandbox   sandboxJSON    `json:"sandbox"`
		Operation map[string]any `json:"operation"`
	}
	if err := c.do(ctx, "POST", sandboxPath(id)+":terminate", map[string]any{}, &resp, requestOptions{idempotent: true}); err != nil {
		return err
	}
	sb := resp.Sandbox
	if wait {
		final, werr := waitSandboxTerminal(ctx, c, id, time.Duration(waitTimeout)*time.Second)
		if final.ID != "" {
			sb = final
		}
		if werr != nil {
			return werr
		}
	}
	if e.emit(map[string]any{"sandbox": sb, "operation": resp.Operation}) {
		return nil
	}
	e.printf("%s %s\n", sb.ID, sb.State)
	return nil
}

// waitSandbox polls until READY or a terminal state.
func waitSandbox(ctx context.Context, c *client, id string, timeout time.Duration) (sandboxJSON, error) {
	return pollSandbox(ctx, c, id, timeout, func(s sandboxJSON) (bool, error) {
		switch s.State {
		case "READY":
			return true, nil
		case "FAILED":
			return true, fmt.Errorf("sandbox %s FAILED: %s", id, orDash(s.StateReason))
		case "FINISHED":
			return true, fmt.Errorf("sandbox %s reached FINISHED before READY", id)
		default:
			return false, nil
		}
	})
}

func waitSandboxTerminal(ctx context.Context, c *client, id string, timeout time.Duration) (sandboxJSON, error) {
	return pollSandbox(ctx, c, id, timeout, func(s sandboxJSON) (bool, error) {
		return s.State == "FINISHED" || s.State == "FAILED", nil
	})
}

func pollSandbox(ctx context.Context, c *client, id string, timeout time.Duration, done func(sandboxJSON) (bool, error)) (sandboxJSON, error) {
	deadline := time.Now().Add(timeout)
	var last sandboxJSON
	for {
		var sb sandboxJSON
		if err := c.do(ctx, "GET", sandboxPath(id), nil, &sb, requestOptions{}); err != nil {
			return last, err
		}
		last = sb
		stop, derr := done(sb)
		if stop || derr != nil {
			return last, derr
		}
		if time.Now().After(deadline) {
			return last, fmt.Errorf("timed out after %s waiting on sandbox %s (state %s)", timeout, id, sb.State)
		}
		if !sleep(ctx) {
			return last, errors.New("interrupted")
		}
	}
}

func renderSandbox(e *env, s sandboxJSON) {
	e.printf("ID:        %s\n", s.ID)
	if s.Name != "" {
		e.printf("Name:      %s\n", s.Name)
	}
	e.printf("Project:   %s\n", s.ProjectID)
	e.printf("State:     %s\n", s.State)
	if s.StateReason != "" {
		e.printf("Reason:    %s\n", s.StateReason)
	}
	e.printf("Image:     %s\n", s.ImageID)
	e.printf("Accel:     %s\n", acceleratorLabel(s))
	e.printf("CPU/Mem:   %dm / %dMiB\n", s.Resources.CPUMilli, s.Resources.MemoryMiB)
	e.printf("Created:   %s\n", orDash(s.CreateTime))
	if s.ReadyTime != "" {
		e.printf("Ready:     %s\n", s.ReadyTime)
	}
	if s.FinishTime != "" {
		e.printf("Finished:  %s\n", s.FinishTime)
	}
	if len(s.Labels) > 0 {
		var pairs []string
		for _, k := range sortedKeys(s.Labels) {
			pairs = append(pairs, k+"="+s.Labels[k])
		}
		e.printf("Labels:    %s\n", strings.Join(pairs, ","))
	}
}

func acceleratorLabel(s sandboxJSON) string {
	t := s.Resources.Accelerator.Type
	if t == "" || t == "NONE" {
		return "none"
	}
	return fmt.Sprintf("%s x%d", t, s.Resources.Accelerator.Count)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
