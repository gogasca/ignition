package probe

import (
	"context"
	"fmt"
	"strings"
	"time"

	"ignition.dev/ignition/internal/id"
)

// ProbeSandboxNamePrefix marks sandboxes the prober creates so a leaked one
// (e.g. a cleanup that failed on a network blip) can be swept later.
const ProbeSandboxNamePrefix = "probe-"

// cpuSandboxReq is the cheapest admissible sandbox: CPU-only, internet
// disabled, a bounded sleep so it stays READY until the journey terminates it.
func cpuSandboxReq(env Env) CreateSandboxReq {
	return CreateSandboxReq{
		Name:    ProbeSandboxNamePrefix + id.New("s")[2:],
		ImageID: env.ImageID,
		Command: []string{"sleep", "300"},
		Resources: &ResourceReq{
			Accelerator: &AcceleratorReq{Count: 0, Type: "NONE"},
		},
		Network: &NetworkReq{InternetAccess: "DISABLED"},
	}
}

// SweepStale terminates prober-created sandboxes older than olderThan that are
// not already terminal. It exists so a run of failed cleanups cannot exhaust the
// project quota and wedge every future cycle. Returns the number swept.
func SweepStale(ctx context.Context, c *Client, olderThan time.Duration) (int, error) {
	list, err := c.ListSandboxes(ctx)
	if err != nil {
		return 0, err
	}
	swept := 0
	for _, sb := range list {
		if !strings.HasPrefix(sb.Name, ProbeSandboxNamePrefix) || sb.Terminal() {
			continue
		}
		if !sb.CreateTime.IsZero() && time.Since(sb.CreateTime) < olderThan {
			continue
		}
		if _, err := c.TerminateSandbox(ctx, sb.ID, id.New("idem")); err == nil {
			swept++
		}
	}
	return swept, nil
}

// waitReady drives a freshly created sandbox to READY, asserting the observed
// state never moves backwards and failing fast on a terminal failure.
func waitReady(ctx context.Context, c *Client, sandboxID string) (SandboxView, error) {
	high := 0
	return c.PollSandbox(ctx, sandboxID, func(sb SandboxView) (bool, error) {
		switch sb.State {
		case "FAILED", "FINISHED":
			return false, fmt.Errorf("sandbox reached %s/%s before READY", sb.State, sb.StateReason)
		case "TERMINATING":
			return false, fmt.Errorf("sandbox is TERMINATING before READY")
		}
		if r := sandboxRank(sb.State); r < high {
			return false, fmt.Errorf("sandbox state went backwards: rank %d after rank %d (%s)", r, high, sb.State)
		} else if r > high {
			high = r
		}
		return sb.State == "READY", nil
	})
}

// terminateAndWait best-effort terminates a sandbox and waits for FINISHED. Used
// both as the final journey step and as deferred cleanup.
func terminateAndWait(ctx context.Context, c *Client, sandboxID string) error {
	if _, err := c.TerminateSandbox(ctx, sandboxID, id.New("idem")); err != nil {
		return err
	}
	_, err := c.PollSandbox(ctx, sandboxID, func(sb SandboxView) (bool, error) {
		return sb.State == "FINISHED" || sb.State == "FAILED", nil
	})
	return err
}

// cleanup terminates a sandbox ignoring errors (deferred best effort).
func cleanup(c *Client, sandboxID string) {
	if sandboxID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	_, _ = c.TerminateSandbox(ctx, sandboxID, id.New("idem"))
}

// --- read-only journeys -------------------------------------------------

var journeyHealth = Journey{
	Name: "health",
	Run: func(ctx context.Context, c *Client, _ Env) ([]Step, error) {
		s := &stepper{}
		return s.steps, s.run("healthz", func() error { return c.Healthz(ctx) })
	},
}

var journeyAuthGuard = Journey{
	Name: "auth-guard",
	Run: func(ctx context.Context, c *Client, _ Env) ([]Step, error) {
		s := &stepper{}
		return s.steps, s.run("reject-anonymous", func() error {
			err := c.Unauthenticated(ctx)
			if err == nil {
				return fmt.Errorf("anonymous request was accepted")
			}
			if !CodeIs(err, "UNAUTHENTICATED") {
				return fmt.Errorf("want UNAUTHENTICATED, got %v", err)
			}
			return nil
		})
	},
}

var journeyDefaultRuntime = Journey{
	Name: "default-runtime",
	Run: func(ctx context.Context, c *Client, _ Env) ([]Step, error) {
		s := &stepper{}
		return s.steps, s.run("get-default-runtime", func() error {
			rt, err := c.DefaultRuntime(ctx)
			if err != nil {
				return err
			}
			if rt.Resources.CPUMilli <= 0 {
				return fmt.Errorf("default runtime cpuMilli = %d", rt.Resources.CPUMilli)
			}
			if rt.Placement.ComputeEnvironment != "STANDARD" {
				return fmt.Errorf("computeEnvironment = %q", rt.Placement.ComputeEnvironment)
			}
			if rt.Network.InternetAccess != "DISABLED" {
				return fmt.Errorf("internetAccess = %q", rt.Network.InternetAccess)
			}
			return nil
		})
	},
}

var journeyList = Journey{
	Name: "list",
	Run: func(ctx context.Context, c *Client, _ Env) ([]Step, error) {
		s := &stepper{}
		return s.steps, s.run("list-sandboxes", func() error {
			_, err := c.ListSandboxes(ctx)
			return err
		})
	},
}

// --- lifecycle journeys ------------------------------------------------

var journeySandboxLifecycle = Journey{
	Name:      "sandbox-lifecycle",
	Lifecycle: true,
	Run: func(ctx context.Context, c *Client, env Env) ([]Step, error) {
		s := &stepper{}
		var sbxID, opID string

		if err := s.run("create", func() error {
			sb, op, err := c.CreateSandbox(ctx, id.New("idem"), cpuSandboxReq(env))
			if err != nil {
				return err
			}
			if sb.State != "CREATING" {
				return fmt.Errorf("new sandbox state = %q", sb.State)
			}
			sbxID, opID = sb.ID, op.ID
			return nil
		}); err != nil {
			return s.steps, err
		}
		defer cleanup(c, sbxID)

		if err := s.run("wait-ready", func() error {
			sb, err := waitReady(ctx, c, sbxID)
			if err != nil {
				return err
			}
			if sb.ReadyTime == nil {
				return fmt.Errorf("READY sandbox has no readyTime")
			}
			return nil
		}); err != nil {
			return s.steps, err
		}

		if err := s.run("operation-succeeded", func() error {
			op, err := c.GetOperation(ctx, opID)
			if err != nil {
				return err
			}
			if op.State != "SUCCEEDED" {
				return fmt.Errorf("create operation state = %q (%s)", op.State, op.ProgressMessage)
			}
			return nil
		}); err != nil {
			return s.steps, err
		}

		if err := s.run("visible-in-list", func() error {
			list, err := c.ListSandboxes(ctx)
			if err != nil {
				return err
			}
			for _, sb := range list {
				if sb.ID == sbxID {
					return nil
				}
			}
			return fmt.Errorf("sandbox %s not in list", sbxID)
		}); err != nil {
			return s.steps, err
		}

		if err := s.run("terminate", func() error {
			return terminateAndWait(ctx, c, sbxID)
		}); err != nil {
			return s.steps, err
		}
		return s.steps, nil
	},
}

var journeyProcessExec = Journey{
	Name:      "process-exec",
	Lifecycle: true,
	Run: func(ctx context.Context, c *Client, env Env) ([]Step, error) {
		s := &stepper{}
		var sbxID, prcID string

		if err := s.run("create-sandbox", func() error {
			sb, _, err := c.CreateSandbox(ctx, id.New("idem"), cpuSandboxReq(env))
			if err != nil {
				return err
			}
			sbxID = sb.ID
			return nil
		}); err != nil {
			return s.steps, err
		}
		defer cleanup(c, sbxID)

		if err := s.run("wait-ready", func() error {
			_, err := waitReady(ctx, c, sbxID)
			return err
		}); err != nil {
			return s.steps, err
		}

		if err := s.run("create-process", func() error {
			p, err := c.CreateProcess(ctx, sbxID, id.New("idem"), []string{"sleep", "30"})
			if err != nil {
				return err
			}
			if p.ID == "" {
				return fmt.Errorf("create-process returned no id")
			}
			prcID = p.ID
			return nil
		}); err != nil {
			return s.steps, err
		}

		if err := s.run("get-and-list-process", func() error {
			got, err := c.GetProcess(ctx, sbxID, prcID)
			if err != nil {
				return err
			}
			if got.ID != prcID {
				return fmt.Errorf("get-process id = %q", got.ID)
			}
			list, err := c.ListProcesses(ctx, sbxID)
			if err != nil {
				return err
			}
			for _, p := range list {
				if p.ID == prcID {
					return nil
				}
			}
			return fmt.Errorf("process %s not in list", prcID)
		}); err != nil {
			return s.steps, err
		}

		// Best-effort: if in-sandbox process supervision is running, confirm the
		// process advances past CREATING. It is not fatal when it does not —
		// exec supervision ships in a later slice, and the control-plane surface
		// (create / attach / signal / cancel) is what this journey guards.
		if err := s.run("observe-process", func() error {
			octx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			p, err := c.PollProcess(octx, sbxID, prcID, func(p ProcessView) (bool, error) {
				if p.State == "FAILED" {
					return false, fmt.Errorf("process entered FAILED")
				}
				return p.State == "RUNNING" || p.State == "STARTING" || p.State == "EXITED", nil
			})
			if err != nil && !errIsDeadline(err) {
				return err
			}
			_ = p
			return ctx.Err() // propagate only a parent-context cancellation
		}); err != nil {
			return s.steps, err
		}

		if err := s.run("attach", func() error {
			att, err := c.AttachProcess(ctx, sbxID, prcID, id.New("idem"))
			if err != nil {
				return err
			}
			if att.StreamToken == "" || att.GatewayURL == "" {
				return fmt.Errorf("attach response missing token or gateway url")
			}
			claims, typ, err := jwtClaims(att.StreamToken)
			if err != nil {
				return fmt.Errorf("stream token not a JWT: %w", err)
			}
			if typ != "stream+jwt" {
				return fmt.Errorf("stream token typ = %q", typ)
			}
			if claims["action"] != "attach" {
				return fmt.Errorf("stream token action = %v", claims["action"])
			}
			if claims["process_id"] != prcID {
				return fmt.Errorf("stream token process_id = %v, want %s", claims["process_id"], prcID)
			}
			return nil
		}); err != nil {
			return s.steps, err
		}

		if err := s.run("signal", func() error {
			_, err := c.SignalProcess(ctx, sbxID, prcID, id.New("idem"), "SIGTERM")
			return err
		}); err != nil {
			return s.steps, err
		}

		if err := s.run("cancel", func() error {
			_, err := c.CancelProcess(ctx, sbxID, prcID, id.New("idem"))
			return err
		}); err != nil {
			return s.steps, err
		}

		if err := s.run("terminate", func() error {
			return terminateAndWait(ctx, c, sbxID)
		}); err != nil {
			return s.steps, err
		}
		return s.steps, nil
	},
}

var journeyIdempotency = Journey{
	Name:      "idempotency",
	Lifecycle: true,
	Run: func(ctx context.Context, c *Client, env Env) ([]Step, error) {
		s := &stepper{}
		key := id.New("idem")
		req := cpuSandboxReq(env)
		var sbxID string

		if err := s.run("create", func() error {
			sb, _, err := c.CreateSandbox(ctx, key, req)
			if err != nil {
				return err
			}
			sbxID = sb.ID
			return nil
		}); err != nil {
			return s.steps, err
		}
		defer cleanup(c, sbxID)

		if err := s.run("replay-same-key", func() error {
			sb, _, err := c.CreateSandbox(ctx, key, req)
			if err != nil {
				return err
			}
			if sb.ID != sbxID {
				return fmt.Errorf("replay returned a different sandbox: %s != %s", sb.ID, sbxID)
			}
			return nil
		}); err != nil {
			return s.steps, err
		}

		if err := s.run("conflict-on-mutated-body", func() error {
			mutated := req
			mutated.Command = []string{"sleep", "7200"}
			_, _, err := c.CreateSandbox(ctx, key, mutated)
			if err == nil {
				return fmt.Errorf("reused key with a changed body was accepted")
			}
			if !CodeIs(err, "IDEMPOTENCY_KEY_REUSED") {
				return fmt.Errorf("want IDEMPOTENCY_KEY_REUSED, got %v", err)
			}
			return nil
		}); err != nil {
			return s.steps, err
		}

		if err := s.run("terminate", func() error {
			return terminateAndWait(ctx, c, sbxID)
		}); err != nil {
			return s.steps, err
		}
		return s.steps, nil
	},
}
