package main

import (
	"testing"
	"time"
)

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
