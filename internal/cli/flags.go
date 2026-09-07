package cli

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"time"
)

// newFlagSet returns a FlagSet that keeps parsing after positionals so global
// flags may appear anywhere, and stays quiet on error (run() formats usage).
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func bindGlobals(fs *flag.FlagSet) *globalFlags {
	g := &globalFlags{output: "text", timeout: 30}
	fs.StringVar(&g.server, "server", "", "API base URL")
	fs.StringVar(&g.project, "project", "", "project ID")
	fs.StringVar(&g.output, "output", "text", "output format: text or json")
	fs.StringVar(&g.output, "o", "text", "output format (shorthand)")
	fs.IntVar(&g.timeout, "timeout", 30, "per-request timeout in seconds")
	return g
}

// parse parses fs (which must have been given bindGlobals) against args,
// allowing flags and positionals to be interleaved (`sandbox terminate sbx_1
// --wait`). It folds any explicitly-set global flags over e.g and returns the
// positional arguments in order.
func (e *env) parse(fs *flag.FlagSet, g *globalFlags, args []string) ([]string, error) {
	var positionals []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return nil, usageErrorf("%v", err)
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		// One positional, then keep parsing in case more flags follow. A "--"
		// terminator makes everything after it positional (flag.Parse already
		// consumed the "--").
		positionals = append(positionals, rest[0])
		rest = rest[1:]
	}
	if err := e.foldGlobals(fs, g); err != nil {
		return nil, err
	}
	return positionals, nil
}

func (e *env) foldGlobals(fs *flag.FlagSet, g *globalFlags) error {
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "server":
			e.g.server = g.server
		case "project":
			e.g.project = g.project
		case "output", "o":
			e.g.output = g.output
		case "timeout":
			e.g.timeout = g.timeout
		}
	})
	if err := validateGlobals(e.g); err != nil {
		return err
	}
	return nil
}

func validateGlobals(g globalFlags) error {
	switch g.output {
	case "text", "json":
	default:
		return usageErrorf("--output must be text or json, got %q", g.output)
	}
	if g.timeout < 1 {
		return usageErrorf("--timeout must be at least 1 second")
	}
	return nil
}

func (e *env) client() (*client, error) {
	return newClient(e.cfg.resolve(e.g), time.Duration(e.g.timeout)*time.Second)
}

// requireProject resolves the effective project or fails with a usage error.
func (e *env) requireProject() (string, error) {
	p := e.cfg.resolve(e.g).Project
	if p == "" {
		return "", usageErrorf("no project set: pass --project, set IGNITION_PROJECT, or run `ignitionctl config set-project <id>`")
	}
	return p, nil
}

// kvSlice collects repeated --flag key=value pairs.
type kvSlice map[string]string

func (k *kvSlice) String() string { return "" }

func (k *kvSlice) Set(v string) error {
	name, val, ok := strings.Cut(v, "=")
	if !ok || name == "" {
		return fmt.Errorf("expected key=value, got %q", v)
	}
	if *k == nil {
		*k = kvSlice{}
	}
	(*k)[name] = val
	return nil
}

// strSlice collects a repeated string flag.
type strSlice []string

func (s *strSlice) String() string { return strings.Join(*s, ",") }

func (s *strSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}
