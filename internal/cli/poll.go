package cli

import (
	"context"
	"time"
)

// pollInterval is how long the --wait / follow / watch loops sleep between
// polls of the control plane. It is a var so tests can shorten it.
var pollInterval = 2 * time.Second

// sleep waits pollInterval or returns false if the context is cancelled first.
func sleep(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(pollInterval):
		return true
	}
}
