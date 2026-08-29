package main

import (
	"fmt"
	"os"

	"ignition.dev/ignition/internal/cli"
)

func main() {
	if err := cli.Execute(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "ignitionctl: %v\n", err)
		os.Exit(1)
	}
}
