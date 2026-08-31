package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// runJourney runs one journey by name against a stub server.
func runJourney(t *testing.T, name string, h http.HandlerFunc) Result {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	c := New(ts.URL, "prj_dev", WithStaticToken("tok"), WithPollInterval(time.Millisecond))
	var j Journey
	for _, cand := range All() {
		if cand.Name == name {
			j = cand
		}
	}
	if j.Name == "" {
		t.Fatalf("unknown journey %q", name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return Run(ctx, c, []Journey{j}, Env{Project: "prj_dev", ImageID: "img_seed"})[0]
}

func TestJourneyHealth(t *testing.T) {
	ok := runJourney(t, "health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	if !ok.OK {
		t.Fatalf("healthy: %v", ok.Err)
	}
	bad := runJourney(t, "health", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	})
	if bad.OK {
		t.Fatal("health journey passed against a 503")
	}
}

func TestJourneyAuthGuard(t *testing.T) {
	// Anonymous request correctly rejected.
	pass := runJourney(t, "auth-guard", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"code":"UNAUTHENTICATED","message":"x"}`))
	})
	if !pass.OK {
		t.Fatalf("auth-guard should pass: %v", pass.Err)
	}

	// API wrongly accepts an anonymous request -> journey MUST fail.
	open := runJourney(t, "auth-guard", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"sandboxes":[]}`))
	})
	if open.OK {
		t.Fatal("auth-guard passed while the API accepted an anonymous request")
	}
	if !strings.Contains(open.Err.Error(), "accepted") {
		t.Fatalf("unexpected error: %v", open.Err)
	}

	// Rejected, but with the wrong code.
	wrong := runJourney(t, "auth-guard", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"code":"PERMISSION_DENIED","message":"x"}`))
	})
	if wrong.OK {
		t.Fatal("auth-guard passed on PERMISSION_DENIED (want UNAUTHENTICATED)")
	}
}

func TestJourneyDefaultRuntime(t *testing.T) {
	good := `{"resources":{"cpuMilli":1000},"placement":{"computeEnvironment":"STANDARD"},"network":{"internetAccess":"DISABLED"}}`
	if r := runJourney(t, "default-runtime", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(good))
	}); !r.OK {
		t.Fatalf("valid runtime rejected: %v", r.Err)
	}

	cases := map[string]string{
		"zero cpu":    `{"resources":{"cpuMilli":0},"placement":{"computeEnvironment":"STANDARD"},"network":{"internetAccess":"DISABLED"}}`,
		"bad compute": `{"resources":{"cpuMilli":1000},"placement":{"computeEnvironment":"BARE_METAL"},"network":{"internetAccess":"DISABLED"}}`,
		"internet on": `{"resources":{"cpuMilli":1000},"placement":{"computeEnvironment":"STANDARD"},"network":{"internetAccess":"ENABLED"}}`,
	}
	for name, body := range cases {
		body := body
		t.Run(name, func(t *testing.T) {
			if r := runJourney(t, "default-runtime", func(w http.ResponseWriter, _ *http.Request) {
				w.Write([]byte(body))
			}); r.OK {
				t.Fatalf("default-runtime passed on %s", name)
			}
		})
	}
}

func TestJourneyListFailsOn500(t *testing.T) {
	r := runJourney(t, "list", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	if r.OK {
		t.Fatal("list journey passed against a 500")
	}
}

func TestSweepStale(t *testing.T) {
	old := time.Now().Add(-time.Hour).Format(time.RFC3339)
	recent := time.Now().Add(-time.Minute).Format(time.RFC3339)
	var terminated []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/sandboxes"):
			w.Write([]byte(`{"sandboxes":[
				{"id":"sbx_stale","name":"probe-aaa","state":"READY","createTime":"` + old + `"},
				{"id":"sbx_fresh","name":"probe-bbb","state":"CREATING","createTime":"` + recent + `"},
				{"id":"sbx_done","name":"probe-ccc","state":"FINISHED","createTime":"` + old + `"},
				{"id":"sbx_user","name":"my-box","state":"READY","createTime":"` + old + `"}
			]}`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, ":terminate"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/projects/prj_dev/sandboxes/")
			id = strings.TrimSuffix(id, ":terminate")
			terminated = append(terminated, id)
			w.WriteHeader(http.StatusAccepted)
			w.Write([]byte(`{"sandbox":{},"operation":{}}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer ts.Close()

	c := New(ts.URL, "prj_dev", WithStaticToken("tok"))
	n, err := SweepStale(context.Background(), c, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || len(terminated) != 1 || terminated[0] != "sbx_stale" {
		t.Fatalf("swept %d %v, want [sbx_stale]", n, terminated)
	}
}

func TestRunRecoversPanic(t *testing.T) {
	j := Journey{Name: "boom", Run: func(context.Context, *Client, Env) ([]Step, error) {
		panic("kaboom")
	}}
	res := Run(context.Background(), New("http://x", "p"), []Journey{j}, Env{})[0]
	if res.OK {
		t.Fatal("panicking journey reported OK")
	}
	if !strings.Contains(res.Err.Error(), "panic") {
		t.Fatalf("err = %v", res.Err)
	}
}
