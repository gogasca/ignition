package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// client is a dependency-free wrapper over the Ignition control-plane REST API.
// It carries the resolved server, bearer token, and default project, and turns
// non-2xx responses into apiError with the server's stable error code.
type client struct {
	server  string
	token   string
	project string
	http    *http.Client
}

func newClient(cfg Config, timeout time.Duration) (*client, error) {
	if cfg.Server == "" {
		return nil, usageErrorf("no server configured: run `ignitionctl login --server <url> --token <token>` or set IGNITION_SERVER")
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &client{
		server:  cfg.Server,
		token:   cfg.Token,
		project: cfg.Project,
		http:    &http.Client{Timeout: timeout},
	}, nil
}

// apiError is a non-2xx response. It maps to a stable process exit code.
type apiError struct {
	Status    int
	Code      string
	Message   string
	RequestID string
	Retryable bool
}

func (e *apiError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	code := e.Code
	if code == "" {
		code = fmt.Sprintf("HTTP_%d", e.Status)
	}
	if e.RequestID != "" {
		return fmt.Sprintf("%s: %s (request %s)", code, msg, e.RequestID)
	}
	return fmt.Sprintf("%s: %s", code, msg)
}

func (e *apiError) exitCode() int {
	switch e.Status {
	case http.StatusUnauthorized:
		return exitAuth
	case http.StatusForbidden:
		return exitDenied
	case http.StatusNotFound:
		return exitNotFound
	default:
		return exitError
	}
}

type requestOptions struct {
	// mutating requests need an Idempotency-Key. If empty and idempotent is
	// true, one is generated per call.
	idempotent bool
	// idempotencyKey pins the key across retries (e.g. --wait polling loops
	// that re-issue the same create).
	idempotencyKey string
	query          map[string]string
}

// do issues one request. out, when non-nil, is JSON-decoded from a 2xx body.
func (c *client) do(ctx context.Context, method, path string, body any, out any, opts requestOptions) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	url := c.server + path
	if len(opts.query) > 0 {
		parts := make([]string, 0, len(opts.query))
		for k, v := range opts.query {
			if v == "" {
				continue
			}
			parts = append(parts, k+"="+v)
		}
		if len(parts) > 0 {
			url += "?" + strings.Join(parts, "&")
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.project != "" {
		req.Header.Set("X-Ignition-Project", c.project)
	}
	if opts.idempotent {
		key := opts.idempotencyKey
		if key == "" {
			key = newIdempotencyKey()
		}
		req.Header.Set("Idempotency-Key", key)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		ae := &apiError{Status: resp.StatusCode, RequestID: resp.Header.Get("X-Request-Id")}
		var sb struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"requestId"`
			Retryable bool   `json:"retryable"`
		}
		if json.Unmarshal(raw, &sb) == nil {
			ae.Code, ae.Message, ae.Retryable = sb.Code, sb.Message, sb.Retryable
			if sb.RequestID != "" {
				ae.RequestID = sb.RequestID
			}
		}
		return ae
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode %s %s response: %w", method, path, err)
		}
	}
	return nil
}

func newIdempotencyKey() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "ictl-" + hex.EncodeToString(b[:])
}

// resource-path helpers. Every route also exists unscoped with the project
// supplied via the X-Ignition-Project header, which is what the client sets.
func sandboxesPath() string            { return "/v1/sandboxes" }
func sandboxPath(id string) string     { return "/v1/sandboxes/" + id }
func processesPath(sbx string) string  { return "/v1/sandboxes/" + sbx + "/processes" }
func processPath(sbx, p string) string { return "/v1/sandboxes/" + sbx + "/processes/" + p }
func operationsPath() string           { return "/v1/operations" }
func operationPath(id string) string   { return "/v1/operations/" + id }
