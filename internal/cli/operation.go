package cli

import (
	"fmt"
)

type operationJSON struct {
	ID              string `json:"id"`
	ProjectID       string `json:"projectId"`
	Kind            string `json:"kind"`
	State           string `json:"state"`
	ResourceID      string `json:"resourceId"`
	CreateTime      string `json:"createTime"`
	StartTime       string `json:"startTime"`
	EndTime         string `json:"endTime"`
	ProgressMessage string `json:"progressMessage"`
}

func cmdOperation(e *env, args []string) error {
	if len(args) == 0 {
		return usageErrorf("usage: ignitionctl operation <list|get|watch|cancel>")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list", "ls":
		return operationList(e, rest)
	case "get", "describe":
		return operationGet(e, rest)
	case "watch":
		return operationWatch(e, rest)
	case "cancel":
		return operationCancel(e, rest)
	default:
		return usageErrorf("unknown operation subcommand %q", sub)
	}
}

func operationList(e *env, args []string) error {
	fs := newFlagSet("operation list")
	g := bindGlobals(fs)
	var resource string
	var limit int
	fs.StringVar(&resource, "resource", "", "filter by resource ID (e.g. a sandbox ID)")
	fs.IntVar(&limit, "limit", 0, "max rows")
	if _, err := e.parse(fs, g, args); err != nil {
		return err
	}
	c, err := e.projectClient()
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	q := map[string]string{}
	if resource != "" {
		q["resourceId"] = resource
	}
	if limit > 0 {
		q["pageSize"] = fmt.Sprint(limit)
	}
	var resp struct {
		Operations    []operationJSON `json:"operations"`
		NextPageToken string          `json:"nextPageToken"`
	}
	if err := c.do(ctx, "GET", c.operationsPath(), nil, &resp, requestOptions{query: q}); err != nil {
		return err
	}
	if e.emit(resp) {
		return nil
	}
	if len(resp.Operations) == 0 {
		e.printf("no operations\n")
		return nil
	}
	rows := make([][]string, 0, len(resp.Operations))
	for _, op := range resp.Operations {
		rows = append(rows, []string{op.ID, op.Kind, op.State, op.ResourceID, ageOf(op.CreateTime)})
	}
	table(e.stdout, []string{"ID", "KIND", "STATE", "RESOURCE", "AGE"}, rows)
	return nil
}

func operationGet(e *env, args []string) error {
	fs := newFlagSet("operation get")
	g := bindGlobals(fs)
	pos, err := e.parse(fs, g, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErrorf("usage: ignitionctl operation get <operation>")
	}
	c, err := e.projectClient()
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	var op operationJSON
	if err := c.do(ctx, "GET", c.operationPath(pos[0]), nil, &op, requestOptions{}); err != nil {
		return err
	}
	if e.emit(op) {
		return nil
	}
	renderOperation(e, op)
	return nil
}

// operationWatch polls GET operation until it reaches a terminal state. The
// server's :watch SSE stream sends a single snapshot then closes, so a poll
// loop is the portable choice for a terminal client.
func operationWatch(e *env, args []string) error {
	fs := newFlagSet("operation watch")
	g := bindGlobals(fs)
	var timeout int
	fs.IntVar(&timeout, "wait-timeout", 600, "seconds to watch before giving up")
	pos, err := e.parse(fs, g, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErrorf("usage: ignitionctl operation watch <operation>")
	}
	id := pos[0]
	c, err := e.projectClient()
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()

	seen := ""
	for i := 0; ; i++ {
		var op operationJSON
		if err := c.do(ctx, "GET", c.operationPath(id), nil, &op, requestOptions{}); err != nil {
			return err
		}
		if op.State != seen {
			seen = op.State
			if !e.g.json() {
				e.printf("%s %s %s\n", op.ID, op.State, op.ProgressMessage)
			}
		}
		switch op.State {
		case "SUCCEEDED", "FAILED", "CANCELLED":
			if e.emit(op) {
				return terminalOpErr(op)
			}
			renderOperation(e, op)
			return terminalOpErr(op)
		}
		if i > timeout/2 {
			return fmt.Errorf("timed out watching operation %s (state %s)", id, op.State)
		}
		if !sleep(ctx) {
			return &exitErr{code: exitError, err: fmt.Errorf("interrupted")}
		}
	}
}

func operationCancel(e *env, args []string) error {
	fs := newFlagSet("operation cancel")
	g := bindGlobals(fs)
	pos, err := e.parse(fs, g, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErrorf("usage: ignitionctl operation cancel <operation>")
	}
	c, err := e.projectClient()
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	var op operationJSON
	if err := c.do(ctx, "POST", c.operationPath(pos[0])+":cancel", map[string]any{}, &op, requestOptions{idempotent: true}); err != nil {
		return err
	}
	if e.emit(op) {
		return nil
	}
	e.printf("%s %s\n", op.ID, op.State)
	return nil
}

func terminalOpErr(op operationJSON) error {
	switch op.State {
	case "FAILED":
		return &exitErr{code: exitError, err: fmt.Errorf("operation %s FAILED: %s", op.ID, orDash(op.ProgressMessage))}
	case "CANCELLED":
		return &exitErr{code: exitError, err: fmt.Errorf("operation %s CANCELLED", op.ID)}
	default:
		return nil
	}
}

func renderOperation(e *env, op operationJSON) {
	e.printf("ID:        %s\n", op.ID)
	e.printf("Kind:      %s\n", op.Kind)
	e.printf("State:     %s\n", op.State)
	e.printf("Resource:  %s\n", orDash(op.ResourceID))
	e.printf("Created:   %s\n", orDash(op.CreateTime))
	if op.EndTime != "" {
		e.printf("Ended:     %s\n", op.EndTime)
	}
	if op.ProgressMessage != "" {
		e.printf("Progress:  %s\n", op.ProgressMessage)
	}
}
