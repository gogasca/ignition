package capacity_test

import (
	"testing"
	"time"

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

func TestP95CreatesPerMinuteIncludesIdleBuckets(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 30, 0, time.UTC)
	created := []time.Time{
		now.Add(-10 * time.Second),
		now.Add(-20 * time.Second),
		now.Add(-70 * time.Second),
		now.Add(-20 * time.Minute), // outside the window
		now.Add(time.Second),       // future clock skew; ignored
	}
	if got := capacity.P95CreatesPerMinute(created, now, 15*time.Minute); got != 2 {
		t.Fatalf("P95CreatesPerMinute = %v, want 2", got)
	}
}

func TestP95CreatesPerMinuteLongIdleWindow(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	// In a 60-minute window, one isolated create is below p95.
	if got := capacity.P95CreatesPerMinute([]time.Time{now}, now, time.Hour); got != 0 {
		t.Fatalf("P95CreatesPerMinute = %v, want 0", got)
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
