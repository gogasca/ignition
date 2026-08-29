package main

import (
	"fmt"
	"os"

	"ignition.dev/ignition/internal/config"
	"ignition.dev/ignition/internal/controller"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ignition-controller: %v\n", err)
		os.Exit(1)
	}
	if err := controller.Run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "ignition-controller: %v\n", err)
		os.Exit(1)
	}
}
