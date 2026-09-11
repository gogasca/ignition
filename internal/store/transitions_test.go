package store

import "testing"

func TestValidObservedTransitionRejectsStaleAndAllowsTermination(t *testing.T) {
	if validObservedTransition("READY", "STARTED") {
		t.Fatal("stale observation regressed READY")
	}
	if validObservedTransition("TERMINATING", "READY") {
		t.Fatal("termination was resurrected")
	}
	if !validObservedTransition("TERMINATING", "FINISHED") {
		t.Fatal("termination could not finish")
	}
	if !validObservedTransition("STARTED", "FAILED") {
		t.Fatal("failure was rejected")
	}
}

func TestValidProcessTransitionRejectsStaleObservation(t *testing.T) {
	if validProcessTransition("RUNNING", "STARTING") {
		t.Fatal("process regressed")
	}
	if validProcessTransition("EXITED", "RUNNING") {
		t.Fatal("exited process resurrected")
	}
	if !validProcessTransition("STARTING", "EXITED") {
		t.Fatal("process exit was rejected")
	}
}
