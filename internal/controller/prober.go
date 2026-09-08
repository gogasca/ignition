package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// ProcObserved is one process's state as reported by its sandbox-init
// supervisor.
type ProcObserved struct {
	State    string `json:"state"`
	ExitCode *int   `json:"exitCode,omitempty"`
	Signal   string `json:"signal,omitempty"`
}

// ProbeResult is one poll of a sandbox's init supervisor.
type ProbeResult struct {
	Processes map[string]ProcObserved
	// IdleSeconds is how long the sandbox has had no running process and no
	// attached exec stream; 0 while active. IdleReported is false for an older
	// supervisor that does not send the field (then idle-timeout enforcement is
	// skipped for that sandbox).
	IdleSeconds  int
	IdleReported bool
}

// ProcessProber fetches observed tenant-process state (and idle time) from a
// sandbox's init supervisor. The controller is the only caller (it holds the
// Pod-network path); sandbox-init exposes GET /v1/processes on port 8081.
type ProcessProber interface {
	Probe(ctx context.Context, podIP string) (ProbeResult, error)
}

// HTTPProber talks to sandbox-init over the Pod network.
type HTTPProber struct {
	Client *http.Client
	Port   int
}

// NewHTTPProber returns a prober with tight timeouts suitable for a reconcile
// loop.
func NewHTTPProber() *HTTPProber {
	return &HTTPProber{
		Client: &http.Client{Timeout: 3 * time.Second},
		Port:   8081,
	}
}

func (p *HTTPProber) Probe(ctx context.Context, podIP string) (ProbeResult, error) {
	port := p.Port
	if port == 0 {
		port = 8081
	}
	url := fmt.Sprintf("http://%s/v1/processes", net.JoinHostPort(podIP, fmt.Sprint(port)))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ProbeResult{}, err
	}
	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return ProbeResult{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return ProbeResult{}, fmt.Errorf("sandbox-init %s: %s", url, resp.Status)
	}
	var out struct {
		Processes   map[string]ProcObserved `json:"processes"`
		IdleSeconds *int                    `json:"idleSeconds"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return ProbeResult{}, err
	}
	res := ProbeResult{Processes: out.Processes}
	if res.Processes == nil {
		res.Processes = map[string]ProcObserved{}
	}
	if out.IdleSeconds != nil {
		res.IdleSeconds = *out.IdleSeconds
		res.IdleReported = true
	}
	return res, nil
}
