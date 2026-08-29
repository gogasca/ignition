package main

import (
	"fmt"
	"os"

	"ignition.dev/ignition/internal/api"
	"ignition.dev/ignition/internal/config"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ignition-api: %v\n", err)
		os.Exit(1)
	}
	if err := api.Run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "ignition-api: %v\n", err)
		os.Exit(1)
	}
}
