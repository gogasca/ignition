package main

import (
	"fmt"
	"os"

	"ignition.dev/ignition/internal/sandboxinit"
)

func main() {
	if err := sandboxinit.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-init: %v\n", err)
		os.Exit(1)
	}
}
