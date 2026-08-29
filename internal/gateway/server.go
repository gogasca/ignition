package gateway

import (
	"fmt"

	"ignition.dev/ignition/internal/config"
)

// Run terminates authenticated exec streams.
func Run(cfg config.Config) error {
	return fmt.Errorf("not implemented (listen %s)", cfg.ListenAddr)
}
