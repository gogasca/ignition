package id_test

import (
	"strings"
	"testing"

	"ignition.dev/ignition/internal/id"
)

func TestNew(t *testing.T) {
	got := id.New("sbx")
	if !strings.HasPrefix(got, "sbx_") {
		t.Fatalf("id = %q", got)
	}
	if len(got) != len("sbx_")+20 {
		t.Fatalf("len = %d id=%q", len(got), got)
	}
	other := id.New("sbx")
	if got == other {
		t.Fatal("ids should be unique")
	}
}
