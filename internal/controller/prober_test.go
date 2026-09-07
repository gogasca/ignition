package controller_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ignition.dev/ignition/internal/controller"
	"ignition.dev/ignition/internal/k8s"
	"ignition.dev/ignition/internal/store"
)

// fakeProber returns canned observed state and records the Pod IPs it was asked
// about.
type fakeProber struct {
	byIP  map[string]map[string]controller.ProcObserved
	asked []string
}

func (f *fakeProber) ObservedProcesses(_ context.Context, podIP string) (map[string]controller.ProcObserved, error) {
	f.asked = append(f.asked, podIP)
	return f.byIP[podIP], nil
}

func TestProcessProberAdvancesState(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	prober := &fakeProber{byIP: map[string]map[string]controller.ProcObserved{}}
	c := controller.New(m, fake, fake, controller.Options{ProcessProber: prober})
	res := admit(t, m, store.TimeoutSpec{})
	ctx := context.Background()

	_ = c.Reconcile(ctx)
	name := k8s.PodName(res.Sandbox.ID)
	fake.SetReady(name, "GPU-1")
	fake.SetPodIP(name, "10.1.2.3")
	_ = c.Reconcile(ctx)

	p, _, err := m.CreateProcess(ctx, store.CreateProcessInput{
		ProjectID: "prj_dev", SandboxID: res.Sandbox.ID, Principal: "alice",
		IdemKey: "p", IdemHash: "ph", Command: []string{"true"},
	})
	if err != nil {
		t.Fatal(err)
	}

	code := 0
	prober.byIP["10.1.2.3"] = map[string]controller.ProcObserved{
		p.ID: {State: "EXITED", ExitCode: &code},
	}
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	got, err := m.GetProcess(ctx, "prj_dev", res.Sandbox.ID, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "EXITED" || got.ExitCode == nil || *got.ExitCode != 0 {
		t.Fatalf("process = %s exit=%v, want EXITED/0", got.State, got.ExitCode)
	}
	if len(prober.asked) == 0 || prober.asked[len(prober.asked)-1] != "10.1.2.3" {
		t.Fatalf("prober was not asked about the Pod IP: %v", prober.asked)
	}
	// The observed annotation should be mirrored for visibility.
	pod, _ := fake.Get(name)
	if !strings.Contains(pod.Annotations[k8s.AnnotProcObserved], "EXITED") {
		t.Fatalf("observed annotation not mirrored: %q", pod.Annotations[k8s.AnnotProcObserved])
	}
}

func TestHTTPProberParsesResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/processes" {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write([]byte(`{"processes":{"prc_1":{"state":"RUNNING"},"prc_2":{"state":"EXITED","exitCode":3}}}`))
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	ip, port, _ := strings.Cut(host, ":")
	p := controller.NewHTTPProber()
	// point the prober at the test server's port
	pp := &controller.HTTPProber{Client: srv.Client(), Port: atoi(port)}
	_ = p

	got, err := pp.ObservedProcesses(context.Background(), ip)
	if err != nil {
		t.Fatal(err)
	}
	if got["prc_1"].State != "RUNNING" {
		t.Fatalf("prc_1 = %+v", got["prc_1"])
	}
	if got["prc_2"].ExitCode == nil || *got["prc_2"].ExitCode != 3 {
		t.Fatalf("prc_2 exit = %v", got["prc_2"].ExitCode)
	}
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}
