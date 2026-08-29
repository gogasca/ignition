package capacity_test

import (
	"testing"

	"ignition.dev/ignition/internal/capacity"
)

func TestDesiredWarmFloor(t *testing.T) {
	got := capacity.DesiredWarm(capacity.Inputs{
		CreatePerMinute:  0,
		NodeProvisionMin: 3,
		MinWarm:          2,
		MaxWarm:          8,
		Safety:           1.3,
	})
	if got != 2 {
		t.Fatalf("DesiredWarm = %d, want 2", got)
	}
}

func TestDesiredWarmFromRate(t *testing.T) {
	got := capacity.DesiredWarm(capacity.Inputs{
		CreatePerMinute:  2,
		NodeProvisionMin: 3,
		MinWarm:          2,
		MaxWarm:          32,
		Safety:           1.3,
	})
	if got != 8 {
		t.Fatalf("DesiredWarm = %d, want 8", got)
	}
}
