package main

import (
	"testing"
	"time"

	"ignition.dev/ignition/internal/probe"
)

func TestStaleAfter(t *testing.T) {
	// Default 5m interval, 10m timeout -> 2*timeout (20m) wins over 3*interval (15m).
	if got := staleAfter(probe.Config{Interval: 5 * time.Minute, Timeout: 10 * time.Minute}); got != 20*time.Minute {
		t.Fatalf("staleAfter = %s, want 20m", got)
	}
	// Long interval, short timeout -> 3*interval wins.
	if got := staleAfter(probe.Config{Interval: 30 * time.Minute, Timeout: time.Minute}); got != 90*time.Minute {
		t.Fatalf("staleAfter = %s, want 90m", got)
	}
}

func TestReadyState(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	staleAfter := 15 * time.Minute

	if _, ok := readyState(0, now, staleAfter); ok {
		t.Fatal("ready before any cycle")
	}
	if _, ok := readyState(now.Add(-time.Minute).Unix(), now, staleAfter); !ok {
		t.Fatal("recent cycle reported not ready")
	}
	if _, ok := readyState(now.Add(-20*time.Minute).Unix(), now, staleAfter); ok {
		t.Fatal("stale cycle reported ready")
	}
}
