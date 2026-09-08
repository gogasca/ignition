//go:build !linux

package sandboxinit

import (
	"errors"
	"os"
)

// openPTY is only implemented for Linux (sandbox-init runs in a Linux
// container). Other platforms fall back to pipes.
func openPTY(rows, cols int) (master, slave *os.File, err error) {
	return nil, nil, errors.New("pty allocation is only supported on linux")
}
