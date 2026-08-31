package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ignition.dev/ignition/internal/id"
)

// Client is a thin HTTP client for the Ignition public v1 API. It is safe for
// concurrent use once constructed.
type Client struct {
	baseURL      string
	project      string
	http         *http.Client
	pollInterval time.Duration
	tokenFn      func(context.Context) (string, error)
}

// Option configures a Client.
type Option func(*Client)

// WithStaticToken sends a fixed bearer on every request.
func WithStaticToken(tok string) Option {
	return func(c *Client) {
		c.tokenFn = func(context.Context) (string, error) { return tok, nil }
	}
}

// WithTokenFunc sets a callback that returns a fresh bearer per request (e.g. a
// cached GCP ID token). A nil result token means "send no Authorization header".
func WithTokenFunc(fn func(context.Context) (string, error)) Option {
	return func(c *Client) { c.tokenFn = fn }
}

// WithHTTPClient overrides the underlying *http.Client.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithPollInterval sets the delay between polls in PollSandbox / PollOperation.
func WithPollInterval(d time.Duration) Option {
	return func(c *Client) { c.pollInterval = d }
}

// New builds a Client for baseURL (e.g. "http://ignition-api.ignition-system:8080")
// scoped to project.
func New(baseURL, project string, opts ...Option) *Client {
	c := &Client{
		baseURL:      strings.TrimRight(baseURL, "/"),
		project:      project,
		http:         &http.Client{Timeout: 30 * time.Second},
		pollInterval: 2 * time.Second,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Project is the project id this client targets.
func (c *Client) Project() string { return c.project }

// APIError is a non-2xx response decoded from the API error envelope.
type APIError struct {
	HTTPStatus        int
	Code              string
	Message           string
	RequestID         string
	RetryAfterSeconds int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("api %d %s: %s (request %s)", e.HTTPStatus, e.Code, e.Message, e.RequestID)
}

// CodeIs reports whether err is an *APIError with the given code.
func CodeIs(err error, code string) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Code == code
}

// HTTPStatus returns the status of err if it is an *APIError, else 0.
func HTTPStatus(err error) int {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.HTTPStatus
	}
	return 0
}

// do issues one request. A non-2xx status yields an *APIError. On 2xx, if out is
// non-nil the body is JSON-decoded into it. idemKey, when non-empty, is sent as
// the Idempotency-Key header. auth controls whether an Authorization header is
// attached.
func (c *Client) do(ctx context.Context, method, path, idemKey string, in, out any, auth bool) (int, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, fmt.Errorf("marshal request: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Request-Id", id.New("probe"))
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	if auth && c.tokenFn != nil {
		tok, err := c.tokenFn(ctx)
		if err != nil {
			return 0, fmt.Errorf("acquire token: %w", err)
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, parseAPIError(resp, raw)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode %s %s: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

func parseAPIError(resp *http.Response, raw []byte) *APIError {
	ae := &APIError{HTTPStatus: resp.StatusCode, RequestID: resp.Header.Get("X-Request-Id")}
	var env struct {
		Code              string `json:"code"`
		Message           string `json:"message"`
		RequestID         string `json:"requestId"`
		RetryAfterSeconds int    `json:"retryAfterSeconds"`
	}
	if json.Unmarshal(raw, &env) == nil && env.Code != "" {
		ae.Code = env.Code
		ae.Message = env.Message
		if env.RequestID != "" {
			ae.RequestID = env.RequestID
		}
		ae.RetryAfterSeconds = env.RetryAfterSeconds
	} else {
		ae.Code = "NON_JSON_ERROR"
		ae.Message = strings.TrimSpace(string(raw))
		if n, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
			ae.RetryAfterSeconds = n
		}
	}
	return ae
}

func (c *Client) proj(suffix string) string {
	return "/v1/projects/" + c.project + suffix
}

// --- Journey-facing calls -------------------------------------------------

// Healthz hits the unauthenticated health endpoint.
func (c *Client) Healthz(ctx context.Context) error {
	code, err := c.do(ctx, http.MethodGet, "/healthz", "", nil, nil, false)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("healthz status %d", code)
	}
	return nil
}

// Unauthenticated issues a GET with no bearer and returns the resulting error
// (expected: *APIError 401 UNAUTHENTICATED).
func (c *Client) Unauthenticated(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodGet, c.proj("/sandboxes"), "", nil, nil, false)
	return err
}

// DefaultRuntime reads the project's default runtime spec.
func (c *Client) DefaultRuntime(ctx context.Context) (RuntimeView, error) {
	var rt RuntimeView
	_, err := c.do(ctx, http.MethodGet, c.proj("/runtimes/default"), "", nil, &rt, true)
	return rt, err
}

// CreateSandbox admits a sandbox. idemKey is required by the API.
func (c *Client) CreateSandbox(ctx context.Context, idemKey string, req CreateSandboxReq) (SandboxView, OperationView, error) {
	var env createEnvelope
	code, err := c.do(ctx, http.MethodPost, c.proj("/sandboxes"), idemKey, req, &env, true)
	if err != nil {
		return SandboxView{}, OperationView{}, err
	}
	if code != http.StatusAccepted {
		return env.Sandbox, env.Operation, fmt.Errorf("create sandbox status %d", code)
	}
	return env.Sandbox, env.Operation, nil
}

// GetSandbox reads one sandbox.
func (c *Client) GetSandbox(ctx context.Context, sandboxID string) (SandboxView, error) {
	var sb SandboxView
	_, err := c.do(ctx, http.MethodGet, c.proj("/sandboxes/"+sandboxID), "", nil, &sb, true)
	return sb, err
}

// ListSandboxes returns the first page of sandboxes.
func (c *Client) ListSandboxes(ctx context.Context) ([]SandboxView, error) {
	var out sandboxList
	_, err := c.do(ctx, http.MethodGet, c.proj("/sandboxes"), "", nil, &out, true)
	return out.Sandboxes, err
}

// TerminateSandbox requests termination. idemKey is required by the API.
func (c *Client) TerminateSandbox(ctx context.Context, sandboxID, idemKey string) (SandboxView, error) {
	var env createEnvelope
	_, err := c.do(ctx, http.MethodPost, c.proj("/sandboxes/"+sandboxID+":terminate"), idemKey, struct{}{}, &env, true)
	return env.Sandbox, err
}

// GetOperation reads one operation.
func (c *Client) GetOperation(ctx context.Context, operationID string) (OperationView, error) {
	var op OperationView
	_, err := c.do(ctx, http.MethodGet, c.proj("/operations/"+operationID), "", nil, &op, true)
	return op, err
}

// CreateProcess starts a process in a READY sandbox. idemKey is required.
func (c *Client) CreateProcess(ctx context.Context, sandboxID, idemKey string, command []string) (ProcessView, error) {
	var p ProcessView
	_, err := c.do(ctx, http.MethodPost, c.proj("/sandboxes/"+sandboxID+"/processes"), idemKey,
		map[string]any{"command": command}, &p, true)
	return p, err
}

// GetProcess reads one process.
func (c *Client) GetProcess(ctx context.Context, sandboxID, processID string) (ProcessView, error) {
	var p ProcessView
	_, err := c.do(ctx, http.MethodGet, c.proj("/sandboxes/"+sandboxID+"/processes/"+processID), "", nil, &p, true)
	return p, err
}

// ListProcesses returns the first page of processes for a sandbox.
func (c *Client) ListProcesses(ctx context.Context, sandboxID string) ([]ProcessView, error) {
	var out struct {
		Processes []ProcessView `json:"processes"`
	}
	_, err := c.do(ctx, http.MethodGet, c.proj("/sandboxes/"+sandboxID+"/processes"), "", nil, &out, true)
	return out.Processes, err
}

// AttachProcess mints a gateway stream token. idemKey is required.
func (c *Client) AttachProcess(ctx context.Context, sandboxID, processID, idemKey string) (AttachView, error) {
	var a AttachView
	_, err := c.do(ctx, http.MethodPost, c.proj("/sandboxes/"+sandboxID+"/processes/"+processID+":attach"), idemKey, struct{}{}, &a, true)
	return a, err
}

// SignalProcess delivers a signal. idemKey is required.
func (c *Client) SignalProcess(ctx context.Context, sandboxID, processID, idemKey, signal string) (ProcessView, error) {
	var p ProcessView
	_, err := c.do(ctx, http.MethodPost, c.proj("/sandboxes/"+sandboxID+"/processes/"+processID+":signal"), idemKey,
		map[string]any{"signal": signal}, &p, true)
	return p, err
}

// CancelProcess requests cancellation. idemKey is required.
func (c *Client) CancelProcess(ctx context.Context, sandboxID, processID, idemKey string) (ProcessView, error) {
	var p ProcessView
	_, err := c.do(ctx, http.MethodPost, c.proj("/sandboxes/"+sandboxID+"/processes/"+processID+":cancel"), idemKey, struct{}{}, &p, true)
	return p, err
}

// CancelOperation cancels an operation. idemKey is required.
func (c *Client) CancelOperation(ctx context.Context, operationID, idemKey string) (OperationView, error) {
	var op OperationView
	_, err := c.do(ctx, http.MethodPost, c.proj("/operations/"+operationID+":cancel"), idemKey, struct{}{}, &op, true)
	return op, err
}

// --- Polling helpers ----------------------------------------------------

// ErrPollTimeout is returned when a poll helper's context expires.
var ErrPollTimeout = errors.New("probe: poll deadline exceeded")

// errIsDeadline reports whether err is a poll/context deadline rather than a
// real failure.
func errIsDeadline(err error) bool {
	return errors.Is(err, ErrPollTimeout) || errors.Is(err, context.DeadlineExceeded)
}

// PollSandbox polls GetSandbox until until returns true, the context is done, or
// the sandbox enters a state the caller did not accept (the until func decides).
func (c *Client) PollSandbox(ctx context.Context, sandboxID string, until func(SandboxView) (bool, error)) (SandboxView, error) {
	t := time.NewTicker(c.pollInterval)
	defer t.Stop()
	var last SandboxView
	for {
		sb, err := c.GetSandbox(ctx, sandboxID)
		if err != nil {
			return last, err
		}
		last = sb
		done, err := until(sb)
		if err != nil {
			return sb, err
		}
		if done {
			return sb, nil
		}
		select {
		case <-ctx.Done():
			return last, fmt.Errorf("%w (last state %s)", ErrPollTimeout, last.State)
		case <-t.C:
		}
	}
}

// PollOperation polls GetOperation until the operation is Done or the context
// expires.
func (c *Client) PollOperation(ctx context.Context, operationID string) (OperationView, error) {
	t := time.NewTicker(c.pollInterval)
	defer t.Stop()
	var last OperationView
	for {
		op, err := c.GetOperation(ctx, operationID)
		if err != nil {
			return last, err
		}
		last = op
		if op.Done() {
			return op, nil
		}
		select {
		case <-ctx.Done():
			return last, fmt.Errorf("%w (last state %s)", ErrPollTimeout, last.State)
		case <-t.C:
		}
	}
}

// PollProcess polls GetProcess until until returns true or the context expires.
func (c *Client) PollProcess(ctx context.Context, sandboxID, processID string, until func(ProcessView) (bool, error)) (ProcessView, error) {
	t := time.NewTicker(c.pollInterval)
	defer t.Stop()
	var last ProcessView
	for {
		p, err := c.GetProcess(ctx, sandboxID, processID)
		if err != nil {
			return last, err
		}
		last = p
		done, err := until(p)
		if err != nil {
			return p, err
		}
		if done {
			return p, nil
		}
		select {
		case <-ctx.Done():
			return last, fmt.Errorf("%w (last state %s)", ErrPollTimeout, last.State)
		case <-t.C:
		}
	}
}
