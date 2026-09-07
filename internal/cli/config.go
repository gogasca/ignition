package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config is the persisted ignitionctl context: which API to talk to, the
// bearer credential, and the default project. It is written to
// $IGNITION_CONFIG or ~/.config/ignition/config.json with 0600 permissions.
type Config struct {
	Server  string `json:"server"`
	Token   string `json:"token,omitempty"`
	Project string `json:"project,omitempty"`
}

func configPath() (string, error) {
	if p := strings.TrimSpace(os.Getenv("IGNITION_CONFIG")); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return "", fmt.Errorf("cannot locate a config directory: %w", err)
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "ignition", "config.json"), nil
}

// loadConfig reads the persisted config. A missing file is not an error; it
// returns the zero Config so first-run commands can still apply env and flags.
func loadConfig() (Config, error) {
	path, err := configPath()
	if err != nil {
		return Config{}, err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return c, nil
}

func saveConfig(c Config) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	// Write via a temp file so a crash never leaves a half-written credential.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// resolve layers env vars and flags over the persisted config. Flags win over
// env, env wins over the file. token has no flag: it comes from the file or
// IGNITION_TOKEN only.
func (c Config) resolve(g globalFlags) Config {
	out := c
	if v := strings.TrimSpace(os.Getenv("IGNITION_SERVER")); v != "" {
		out.Server = v
	}
	if v := strings.TrimSpace(os.Getenv("IGNITION_TOKEN")); v != "" {
		out.Token = v
	}
	if v := strings.TrimSpace(os.Getenv("IGNITION_PROJECT")); v != "" {
		out.Project = v
	}
	if g.server != "" {
		out.Server = g.server
	}
	if g.project != "" {
		out.Project = g.project
	}
	out.Server = strings.TrimRight(out.Server, "/")
	return out
}
