//go:build !cgo

package main

import (
	"fmt"
	"os"
)

// This stub keeps `go build ./...` / `go vet ./...` green under the repo's
// default CGO_ENABLED=0. The real probe (main_cgo.go) is built with
// CGO_ENABLED=1 into the GPU sandbox-init image.
func main() {
	fmt.Fprintln(os.Stderr, "cuda-check: built without cgo; rebuild with CGO_ENABLED=1")
	os.Exit(2)
}
