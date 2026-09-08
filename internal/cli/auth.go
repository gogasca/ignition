package cli

import (
	"fmt"
	"strings"
)

func cmdLogin(e *env, args []string) error {
	// login owns --server/--project (it persists them), so it does not use the
	// shared bindGlobals set.
	fs := newFlagSet("login")
	var token, server, project string
	fs.StringVar(&token, "token", "", "bearer token to store (dev bearer or a pre-issued access JWT)")
	fs.StringVar(&server, "server", "", "API base URL")
	fs.StringVar(&project, "project", "", "default project ID")
	if err := fs.Parse(args); err != nil {
		return usageErrorf("%v", err)
	}
	if server == "" {
		server = e.g.server
	}
	if project == "" {
		project = e.g.project
	}

	cfg := e.cfg
	if server != "" {
		cfg.Server = strings.TrimRight(server, "/")
	}
	if cfg.Server == "" {
		return usageErrorf("--server is required on first login")
	}
	if project != "" {
		cfg.Project = project
	}
	if token != "" {
		cfg.Token = token
	}
	if cfg.Token == "" {
		return usageErrorf("--token is required (interactive OIDC device login is not implemented yet)")
	}

	if err := saveConfig(cfg); err != nil {
		return &exitErr{code: exitError, err: err}
	}
	e.cfg = cfg

	// Confirm the credential works and surface the principal.
	c, err := e.client()
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	var me map[string]any
	if err := c.do(ctx, "GET", "/v1/me", nil, &me, requestOptions{}); err != nil {
		return fmt.Errorf("stored config, but the token was rejected: %w", err)
	}
	path, _ := configPath()
	e.printf("Logged in to %s as %v (config: %s)\n", cfg.Server, me["subject"], path)
	return nil
}

func cmdLogout(e *env, args []string) error {
	fs := newFlagSet("logout")
	g := bindGlobals(fs)
	if _, err := e.parse(fs, g, args); err != nil {
		return err
	}
	cfg := e.cfg
	cfg.Token = ""
	if err := saveConfig(cfg); err != nil {
		return &exitErr{code: exitError, err: err}
	}
	e.printf("Removed stored token; server and project retained.\n")
	return nil
}

func cmdWhoami(e *env, args []string) error {
	fs := newFlagSet("whoami")
	g := bindGlobals(fs)
	if _, err := e.parse(fs, g, args); err != nil {
		return err
	}
	c, err := e.client()
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	var me map[string]any
	if err := c.do(ctx, "GET", "/v1/me", nil, &me, requestOptions{}); err != nil {
		return err
	}
	if e.emit(me) {
		return nil
	}
	e.printf("subject: %v\n", me["subject"])
	if v, ok := me["domain"]; ok && v != "" {
		e.printf("domain:  %v\n", v)
	}
	if v, ok := me["client"]; ok && v != "" {
		e.printf("client:  %v\n", v)
	}
	return nil
}

func cmdProjects(e *env, args []string) error {
	fs := newFlagSet("projects")
	g := bindGlobals(fs)
	if _, err := e.parse(fs, g, args); err != nil {
		return err
	}
	// The Project API is not exposed (projects are seeded server-side), so there
	// is nothing to list. Report the configured context instead.
	cfg := e.cfg.resolve(e.g)
	if e.emit(map[string]any{"project": cfg.Project, "server": cfg.Server}) {
		return nil
	}
	if cfg.Project == "" {
		e.printf("no project configured; the Project API is not exposed, so set one with `ignitionctl config set-project <id>`\n")
		return nil
	}
	e.printf("current project: %s\n", cfg.Project)
	e.printf("(the Project API is not exposed; project IDs are provisioned server-side)\n")
	return nil
}

func cmdConfig(e *env, args []string) error {
	fs := newFlagSet("config")
	g := bindGlobals(fs)
	args, err := e.parse(fs, g, args)
	if err != nil {
		return err
	}
	cfg := e.cfg.resolve(e.g)
	if len(args) == 0 {
		if e.emit(cfg.redacted()) {
			return nil
		}
		path, _ := configPath()
		e.printf("server:  %s\n", cfg.Server)
		e.printf("project: %s\n", cfg.Project)
		e.printf("token:   %s\n", tokenStatus(cfg.Token))
		e.printf("config:  %s\n", path)
		return nil
	}
	switch args[0] {
	case "set-project":
		if len(args) != 2 {
			return usageErrorf("usage: ignitionctl config set-project <id>")
		}
		stored := e.cfg
		stored.Project = args[1]
		if err := saveConfig(stored); err != nil {
			return &exitErr{code: exitError, err: err}
		}
		e.printf("default project set to %s\n", args[1])
		return nil
	case "set-server":
		if len(args) != 2 {
			return usageErrorf("usage: ignitionctl config set-server <url>")
		}
		stored := e.cfg
		stored.Server = strings.TrimRight(args[1], "/")
		if err := saveConfig(stored); err != nil {
			return &exitErr{code: exitError, err: err}
		}
		e.printf("server set to %s\n", stored.Server)
		return nil
	default:
		return usageErrorf("unknown config subcommand %q", args[0])
	}
}

func (c Config) redacted() Config {
	c.Token = tokenStatus(c.Token)
	return c
}

func tokenStatus(tok string) string {
	if tok == "" {
		return "(none)"
	}
	return "(set)"
}
