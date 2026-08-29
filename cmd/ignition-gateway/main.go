package main

import (
	"fmt"
	"os"

	"ignition.dev/ignition/internal/config"
	"ignition.dev/ignition/internal/gateway"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ignition-gateway: %v\n", err)
		os.Exit(1)
	}
	if err := gateway.Run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "ignition-gateway: %v\n", err)
		os.Exit(1)
	}
}
