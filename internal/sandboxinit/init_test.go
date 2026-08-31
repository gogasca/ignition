package sandboxinit

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthAndReadiness(t *testing.T) {
	s := New(func() (string, error) { return "GPU-abc", nil })
	health := httptest.NewRecorder()
	s.Handler().ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health status = %d", health.Code)
	}
	ready := httptest.NewRecorder()
	s.Handler().ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusOK || ready.Header().Get("X-Ignition-GPU-UUID") != "GPU-abc" {
		t.Fatalf("ready status=%d uuid=%q", ready.Code, ready.Header().Get("X-Ignition-GPU-UUID"))
	}
}

func TestReadinessFailsClosedWithoutGPU(t *testing.T) {
	s := New(func() (string, error) { return "", errors.New("missing") })
	r := httptest.NewRecorder()
	s.Handler().ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", r.Code)
	}
}

func TestReadinessRetriesAfterDeviceInjection(t *testing.T) {
	calls := 0
	s := New(func() (string, error) {
		calls++
		if calls == 1 {
			return "", errors.New("not injected")
		}
		return "GPU-late", nil
	})
	for want, status := range []int{http.StatusServiceUnavailable, http.StatusOK} {
		r := httptest.NewRecorder()
		s.Handler().ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if r.Code != status {
			t.Fatalf("probe %d status=%d want=%d", want, r.Code, status)
		}
	}
}

func TestDetectGPUFromEnvironment(t *testing.T) {
	t.Setenv("NVIDIA_VISIBLE_DEVICES", "GPU-one")
	if got, err := detectGPUFromEnvironment(); err != nil || got != "GPU-one" {
		t.Fatalf("got %q err=%v", got, err)
	}
	t.Setenv("NVIDIA_VISIBLE_DEVICES", "GPU-one,GPU-two")
	if _, err := detectGPUFromEnvironment(); err == nil {
		t.Fatal("multiple GPUs accepted")
	}
}

func TestCPUReadinessNeedsNoAccelerator(t *testing.T) {
	s := New(detectNoAccelerator)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("CPU /readyz = %d, want 200", rec.Code)
	}
}
