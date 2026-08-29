package store_test

import (
	"testing"

	"ignition.dev/ignition/internal/store"
)

func TestValidImageID(t *testing.T) {
	ok := []string{"img_seed", "img", "a", "A1._-z"}
	for _, id := range ok {
		if err := store.CheckImageID(id); err != nil {
			t.Fatalf("%q: %v", id, err)
		}
	}
	bad := []string{"", "/etc/passwd", "a/b", "img@sha256:x", "reg:tag", "../x", " "}
	for _, id := range bad {
		if store.CheckImageID(id) == nil {
			t.Fatalf("%q accepted", id)
		}
	}
}
