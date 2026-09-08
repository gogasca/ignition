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
	idle  map[string]int
	asked []string
}

func (f *fakeProber) Probe(_ context.Context, podIP string) (controller.ProbeResult, error) {
	f.asked = append(f.asked, podIP)
	res := controller.ProbeResult{Processes: f.byIP[podIP]}
	if f.idle != nil {
		if s, ok := f.idle[podIP]; ok {
			res.IdleSeconds = s
			res.IdleReported = true
		}
	}
	return res, nil
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

func TestIdleTimeoutTerminatesReadySandbox(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	prober := &fakeProber{
		byIP: map[string]map[string]controller.ProcObserved{},
		idle: map[string]int{},
	}
	c := controller.New(m, fake, fake, controller.Options{ProcessProber: prober})
	res := admit(t, m, store.TimeoutSpec{IdleSeconds: 600})
	ctx := context.Background()

	_ = c.Reconcile(ctx)
	name := k8s.PodName(res.Sandbox.ID)
	fake.SetReady(name, "GPU-1")
	fake.SetPodIP(name, "10.1.2.3")

	// Not idle long enough yet.
	prober.idle["10.1.2.3"] = 120
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if sb := mustGet(t, m, res.Sandbox.ID); sb.State != "READY" {
		t.Fatalf("state = %s, want READY", sb.State)
	}

	// Idle past the threshold: the controller terminates it and deletes the Pod.
	prober.idle["10.1.2.3"] = 600
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	sb := mustGet(t, m, res.Sandbox.ID)
	if sb.State != "FINISHED" || sb.StateReason != "IDLE_TIMEOUT" {
		t.Fatalf("state = %s/%s, want FINISHED/IDLE_TIMEOUT", sb.State, sb.StateReason)
	}
	if _, err := fake.Get(name); err == nil {
		t.Fatalf("sandbox Pod %s was not deleted", name)
	}
}

func TestIdleTimeoutSkippedWhenOptedOut(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	prober := &fakeProber{
		byIP: map[string]map[string]controller.ProcObserved{},
		idle: map[string]int{"10.1.2.3": 99999},
	}
	c := controller.New(m, fake, fake, controller.Options{ProcessProber: prober})
	res := admit(t, m, store.TimeoutSpec{IdleSeconds: 0}) // opted out
	ctx := context.Background()
	_ = c.Reconcile(ctx)
	name := k8s.PodName(res.Sandbox.ID)
	fake.SetReady(name, "GPU-1")
	fake.SetPodIP(name, "10.1.2.3")
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if sb := mustGet(t, m, res.Sandbox.ID); sb.State != "READY" {
		t.Fatalf("state = %s, want READY (idle disabled)", sb.State)
	}
}

func TestRuntimeLimitExceeded(t *testing.T) {
	m := store.NewMemory()
	fake := k8s.NewFake()
	c := controller.New(m, fake, fake, controller.Options{})
	res := admit(t, m, store.TimeoutSpec{})
	ctx := context.Background()
	_ = c.Reconcile(ctx)
	name := k8s.PodName(res.Sandbox.ID)
	fake.SetReady(name, "GPU-1")
	_ = c.Reconcile(ctx)

	// kubelet fails the Pod with DeadlineExceeded when activeDeadlineSeconds hits.
	fake.SetFailed(name, "DeadlineExceeded")
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	sb := mustGet(t, m, res.Sandbox.ID)
	if sb.State != "FAILED" || sb.StateReason != "RUNTIME_LIMIT_EXCEEDED" {
		t.Fatalf("state = %s/%s, want FAILED/RUNTIME_LIMIT_EXCEEDED", sb.State, sb.StateReason)
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
	pp := &controller.HTTPProber{Client: srv.Client(), Port: atoi(port)}

	got, err := pp.Probe(context.Background(), ip)
	if err != nil {
		t.Fatal(err)
	}
	if got.Processes["prc_1"].State != "RUNNING" {
		t.Fatalf("prc_1 = %+v", got.Processes["prc_1"])
	}
	if got.Processes["prc_2"].ExitCode == nil || *got.Processes["prc_2"].ExitCode != 3 {
		t.Fatalf("prc_2 exit = %v", got.Processes["prc_2"].ExitCode)
	}
}

func TestHTTPProberParsesIdleSeconds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"processes":{},"idleSeconds":42}`))
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	ip, port, _ := strings.Cut(host, ":")
	pp := &controller.HTTPProber{Client: srv.Client(), Port: atoi(port)}

	got, err := pp.Probe(context.Background(), ip)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IdleReported || got.IdleSeconds != 42 {
		t.Fatalf("idle = %d reported=%v, want 42/true", got.IdleSeconds, got.IdleReported)
	}
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}
