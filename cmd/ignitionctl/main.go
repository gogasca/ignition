package main

import (
	"fmt"
	"os"

	"ignition.dev/ignition/internal/cli"
)

func main() {
	err := cli.Execute(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "ignitionctl: %v\n", err)
	}
	os.Exit(cli.ExitCode(err))
}
