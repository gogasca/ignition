package id

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// New returns prefix_ plus 20 hex characters, e.g. sbx_a1b2...
func New(prefix string) string {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Errorf("id: entropy: %w", err))
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}
