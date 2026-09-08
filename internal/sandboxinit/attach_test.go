package sandboxinit

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A valid attach can land a beat before the desired-process set reaches the
// supervisor (downwardAPI projection + one reconcile tick). The handler must
// wait for it rather than 404 the caller into the polling fallback.
func TestAttachWaitsForLateProcess(t *testing.T) {
	old := attachWait
	attachWait = 2 * time.Second
	defer func() { attachWait = old }()

	pm, df := newManager(t)
	s := New(func() (string, error) { return "", nil }).WithProcessManager(pm)

	go func() {
		time.Sleep(150 * time.Millisecond)
		writeDesired(t, df, map[string]desired{
			"prc_late": {Command: []string{"sh", "-c", "sleep 1"}},
		})
		pm.reconcileOnce()
	}()

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/processes/prc_late/attach", nil)
	s.Handler().ServeHTTP(w, r)

	// The websocket upgrade fails against a ResponseRecorder, but the point is
	// that the handler got past the 404 gate for a process it had not yet seen.
	if w.Code == http.StatusNotFound {
		t.Fatalf("attach 404'd a process that appeared during the wait window")
	}
}

func TestAttachGivesUpWhenProcessNeverAppears(t *testing.T) {
	old := attachWait
	attachWait = 200 * time.Millisecond
	defer func() { attachWait = old }()

	pm, _ := newManager(t)
	s := New(func() (string, error) { return "", nil }).WithProcessManager(pm)

	start := time.Now()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/processes/prc_missing/attach", nil))

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if elapsed := time.Since(start); elapsed < attachWait {
		t.Fatalf("returned after %s, expected to wait at least %s", elapsed, attachWait)
	}
}
