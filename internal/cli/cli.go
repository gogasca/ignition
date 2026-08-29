package cli

import "fmt"

func Execute(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: ignitionctl <auth|sandbox|operation> ...")
	}
	return fmt.Errorf("not implemented: %s", args[0])
}
