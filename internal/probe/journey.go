package probe

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Env carries the knobs a journey needs.
type Env struct {
	Project string
	ImageID string
}

// Step is one timed stage within a journey.
type Step struct {
	Name string
	Dur  time.Duration
}

// Result is the outcome of running one journey.
type Result struct {
	Journey string
	OK      bool
	Dur     time.Duration
	Steps   []Step
	Err     error
}

// Journey is a named critical user journey.
type Journey struct {
	Name string
	// Lifecycle journeys create a real sandbox (cost + blast radius).
	Lifecycle bool
	Run       func(ctx context.Context, c *Client, env Env) ([]Step, error)
}

// stepper accumulates timed steps for a journey body.
type stepper struct {
	steps []Step
}

// run times fn as a named step. On error it stops the journey.
func (s *stepper) run(name string, fn func() error) error {
	start := time.Now()
	err := fn()
	s.steps = append(s.steps, Step{Name: name, Dur: time.Since(start)})
	if err != nil {
		return fmt.Errorf("step %q: %w", name, err)
	}
	return nil
}

// All returns every journey in a stable order.
func All() []Journey {
	return []Journey{
		journeyHealth,
		journeyAuthGuard,
		journeyDefaultRuntime,
		journeyList,
		journeySandboxLifecycle,
		journeyProcessExec,
		journeyIdempotency,
	}
}

// Select resolves a spec ("full", "lite", or a comma list of names) to journeys.
func Select(spec string) ([]Journey, error) {
	spec = strings.TrimSpace(spec)
	all := All()
	switch spec {
	case "", "full", "all":
		return all, nil
	case "lite", "read", "readonly":
		var out []Journey
		for _, j := range all {
			if !j.Lifecycle {
				out = append(out, j)
			}
		}
		return out, nil
	}
	byName := map[string]Journey{}
	for _, j := range all {
		byName[j.Name] = j
	}
	var out []Journey
	for _, name := range strings.Split(spec, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		j, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("unknown journey %q (known: %s)", name, strings.Join(names(all), ", "))
		}
		out = append(out, j)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no journeys selected from %q", spec)
	}
	return out, nil
}

func names(js []Journey) []string {
	out := make([]string, len(js))
	for i, j := range js {
		out[i] = j.Name
	}
	sort.Strings(out)
	return out
}

// Run executes each journey, timing it and recovering panics into Result.Err.
func Run(ctx context.Context, c *Client, js []Journey, env Env) []Result {
	out := make([]Result, 0, len(js))
	for _, j := range js {
		out = append(out, runOne(ctx, c, j, env))
	}
	return out
}

func runOne(ctx context.Context, c *Client, j Journey, env Env) (res Result) {
	res = Result{Journey: j.Name}
	start := time.Now()
	defer func() {
		res.Dur = time.Since(start)
		if r := recover(); r != nil {
			res.OK = false
			res.Err = fmt.Errorf("panic: %v", r)
		}
	}()
	steps, err := j.Run(ctx, c, env)
	res.Steps = steps
	res.Err = err
	res.OK = err == nil
	return res
}

// AnyFailed reports whether any result failed.
func AnyFailed(results []Result) bool {
	for _, r := range results {
		if !r.OK {
			return true
		}
	}
	return false
}
