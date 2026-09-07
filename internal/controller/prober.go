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

// ProcessProber fetches observed tenant-process state from a sandbox's
// init supervisor. The controller is the only caller (it holds the Pod-network
// path); sandbox-init exposes GET /v1/processes on port 8081.
type ProcessProber interface {
	ObservedProcesses(ctx context.Context, podIP string) (map[string]ProcObserved, error)
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

func (p *HTTPProber) ObservedProcesses(ctx context.Context, podIP string) (map[string]ProcObserved, error) {
	port := p.Port
	if port == 0 {
		port = 8081
	}
	url := fmt.Sprintf("http://%s/v1/processes", net.JoinHostPort(podIP, fmt.Sprint(port)))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sandbox-init %s: %s", url, resp.Status)
	}
	var out struct {
		Processes map[string]ProcObserved `json:"processes"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	if out.Processes == nil {
		out.Processes = map[string]ProcObserved{}
	}
	return out.Processes, nil
}
