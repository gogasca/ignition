package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ignition.dev/ignition/internal/auth"
	"ignition.dev/ignition/internal/store"
)

type createSandboxBody struct {
	Name             string               `json:"name"`
	ImageID          string               `json:"imageId"`
	Command          []string             `json:"command"`
	Args             []string             `json:"args"`
	WorkingDirectory string               `json:"workingDirectory"`
	NativeEntrypoint bool                 `json:"nativeEntrypoint"`
	Resources        *store.ResourceSpec  `json:"resources"`
	Placement        *store.PlacementSpec `json:"placement"`
	Timeouts         *store.TimeoutSpec   `json:"timeouts"`
	Network          *store.NetworkSpec   `json:"network"`
	Labels           map[string]string    `json:"labels"`
	SecretRefs       []store.SecretRef    `json:"secretRefs"`
}

func (s *Server) createSandbox(w http.ResponseWriter, r *http.Request) {
	rid := s.requestID(r.Context())
	project := r.PathValue("project")
	if !s.authorize(w, r, project, auth.PermSandboxCreate, false) {
		return
	}
	key, ok := requireIdempotency(w, rid, r)
	if !ok {
		return
	}
	raw, err := readBody(w, r, 1<<20)
	if err != nil {
		writeStatus(w, rid, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error(), false, 0)
		return
	}
	in, err := s.parseCreate(raw)
	if err != nil {
		writeStatus(w, rid, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error(), false, 0)
		return
	}
	hash := canonicalHash(r.Method, r.URL.Path, raw)
	res, err := s.store.CreateSandbox(r.Context(), store.CreateSandboxInput{
		ProjectID:        project,
		Principal:        s.principal(r.Context()).Subject,
		IdemKey:          key,
		IdemHash:         hash,
		Name:             in.Name,
		ImageID:          in.ImageID,
		Command:          in.Command,
		Args:             in.Args,
		MainCommand:      in.MainCommand,
		WorkingDir:       in.WorkingDir,
		NativeEntrypoint: in.NativeEntrypoint,
		Resources:        in.Resources,
		Placement:        in.Placement,
		Timeouts:         in.Timeouts,
		Network:          in.Network,
		Labels:           in.Labels,
		SecretRefs:       in.SecretRefs,
		TraceID:          rid,
		MaxActive:        s.cfg.MaxActiveSandboxes,
		Admit: func(_ context.Context, img store.Image) error {
			return admitEagerPull(img, in.Timeouts.StartupSeconds, in.ImageID, s.cfg.AssumedEagerPullMBps)
		},
	})
	if err != nil {
		writeStoreError(w, rid, err)
		return
	}
	if res.Replay != nil {
		writeReplay(w, res.Replay)
		return
	}
	w.Header().Set("Location", fmt.Sprintf("/v1/projects/%s/sandboxes/%s", project, res.Sandbox.ID))
	w.Header().Set("Retry-After", "1")
	writeJSON(w, http.StatusAccepted, map[string]any{"sandbox": res.Sandbox, "operation": res.Operation})
}

// admitEagerPull rejects a create before it can consume a warm-node cycle and
// quota only when the image is known (from admission) to be
// streaming-ineligible and its estimated eager pull time exceeds the
// request's own startupSeconds — see
// docs/design/ignition-image-delivery.md: "If its measured pull
// cannot fit the sandbox startup deadline, creation fails early... rather
// than remaining ambiguously stuck." The estimate is not a measurement: no
// launch has ever been observed in this deployment, so this is a
// conservative size/assumed-bandwidth calculation only (see
// Config.AssumedEagerPullMBps), and an eligible/unsized image always passes —
// this check only ever adds a fast, explicit failure for a case that would
// otherwise silently time out later.
//
// It runs as CreateSandboxInput.Admit, i.e. inside the idempotent transaction
// on the non-replay path only, so a retried request keeps its original
// outcome even if the image's streaming-eligibility changed in between.
func admitEagerPull(img store.Image, startupSeconds int, imageID string, mbps float64) error {
	if img.StreamingEligible || img.CompressedBytes <= 0 {
		return nil
	}
	estimate := estimatedEagerPullSeconds(img.CompressedBytes, mbps)
	deadline := float64(startupSeconds)
	if deadline <= 0 || estimate <= deadline {
		return nil
	}
	return &store.AdmissionError{
		Code: "IMAGE_UNAVAILABLE",
		Message: fmt.Sprintf(
			"image %q is not eligible for GKE image streaming (%s) and its estimated eager pull (%.0fs, at an assumed %.0f MB/s) exceeds startupSeconds (%.0fs)",
			imageID, img.IneligibleReason, estimate, mbps, deadline,
		),
	}
}

// estimatedEagerPullSeconds is a conservative estimate only: compressedBytes
// is real (measured at admission), mbps is an assumed placeholder throughput
// (Config.AssumedEagerPullMBps), not a measurement of any actual pull.
func estimatedEagerPullSeconds(compressedBytes int64, mbps float64) float64 {
	return float64(compressedBytes) / (mbps * 1_000_000)
}

func (s *Server) parseCreate(raw []byte) (store.CreateSandboxInput, error) {
	var body createSandboxBody
	if err := decodeJSON(raw, &body); err != nil {
		return store.CreateSandboxInput{}, fmt.Errorf("invalid JSON")
	}
	if body.ImageID == "" {
		return store.CreateSandboxInput{}, fmt.Errorf("imageId is required")
	}
	if err := store.CheckImageID(body.ImageID); err != nil {
		return store.CreateSandboxInput{}, err
	}
	if err := checkCommand(body.Command); err != nil {
		return store.CreateSandboxInput{}, err
	}
	if err := checkCommand(body.Args); err != nil {
		return store.CreateSandboxInput{}, fmt.Errorf("args: %w", err)
	}
	if err := checkLabels(body.Labels); err != nil {
		return store.CreateSandboxInput{}, err
	}
	if err := checkSecretRefs(body.SecretRefs); err != nil {
		return store.CreateSandboxInput{}, err
	}
	for k := range body.Labels {
		if strings.HasPrefix(k, "ignition.") {
			return store.CreateSandboxInput{}, fmt.Errorf("label key %q is reserved", k)
		}
	}

	// Every RuntimeSpec field is optional: build a partial spec from the
	// request and merge it over the system default runtime.
	region := s.cfg.EnabledRegion
	if region == "" {
		region = "us-central1"
	}
	var req store.RuntimeSpec
	if body.Resources != nil {
		req.Resources = *body.Resources
	}
	if body.Timeouts != nil {
		req.Timeouts = *body.Timeouts
	}
	if body.Network != nil {
		req.Network = *body.Network
	}
	if body.Placement != nil {
		if body.Placement.Region != "" && body.Placement.Region != region {
			return store.CreateSandboxInput{}, fmt.Errorf("region %q is not enabled", body.Placement.Region)
		}
		req.Placement = *body.Placement
	}
	rt := store.MergeRuntime(s.cfg.EffectiveDefaultRuntime(), req)
	rt.Placement.Region = region
	if err := store.ValidateRuntimeSpec(rt); err != nil {
		return store.CreateSandboxInput{}, err
	}
	if !s.cfg.AcceleratorAllowed(rt.Resources.Accelerator.Type) {
		return store.CreateSandboxInput{}, fmt.Errorf("accelerator.type %q is not allowed", rt.Resources.Accelerator.Type)
	}

	// Managed (nativeEntrypoint=false): append(command, args...) is the argv of
	// the sandbox's main supervised process, which CreateSandbox inserts as a
	// processes row. Native: command/args pass straight to the container spec
	// (internal/k8s) with Kubernetes command/args semantics.
	var mainCommand []string
	if !body.NativeEntrypoint && (len(body.Command) > 0 || len(body.Args) > 0) {
		mainCommand = append(append([]string{}, body.Command...), body.Args...)
		if err := checkCommand(mainCommand); err != nil {
			return store.CreateSandboxInput{}, fmt.Errorf("command + args: %w", err)
		}
	}

	return store.CreateSandboxInput{
		Name:             body.Name,
		ImageID:          body.ImageID,
		Command:          body.Command,
		Args:             body.Args,
		MainCommand:      mainCommand,
		WorkingDir:       body.WorkingDirectory,
		NativeEntrypoint: body.NativeEntrypoint,
		Resources:        rt.Resources,
		Placement:        rt.Placement,
		Timeouts:         rt.Timeouts,
		Network:          rt.Network,
		Labels:           body.Labels,
		SecretRefs:       body.SecretRefs,
	}, nil
}

func (s *Server) getSandbox(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if !s.authorize(w, r, project, auth.PermSandboxGet, false) {
		return
	}
	sb, err := s.store.GetSandbox(r.Context(), project, r.PathValue("sandbox"))
	if err != nil {
		writeStoreError(w, s.requestID(r.Context()), err)
		return
	}
	writeJSON(w, http.StatusOK, sb)
}

func (s *Server) listSandboxes(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if !s.authorize(w, r, project, auth.PermSandboxGet, false) {
		return
	}
	size, ok := pageSize(w, r, s.requestID(r.Context()))
	if !ok {
		return
	}
	items, next, err := s.store.ListSandboxes(r.Context(), project, size, r.URL.Query().Get("pageToken"))
	if err != nil {
		writeStoreError(w, s.requestID(r.Context()), err)
		return
	}
	if items == nil {
		items = []store.Sandbox{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sandboxes": items, "nextPageToken": next})
}

func (s *Server) terminateSandbox(w http.ResponseWriter, r *http.Request) {
	rid := s.requestID(r.Context())
	project := r.PathValue("project")
	sandboxID := r.PathValue("sandbox")
	key, ok := requireIdempotency(w, rid, r)
	if !ok {
		return
	}
	if !s.authorize(w, r, project, auth.PermSandboxGet, false) {
		return
	}
	sb, err := s.store.GetSandbox(r.Context(), project, sandboxID)
	if err != nil {
		writeStoreError(w, rid, err)
		return
	}
	p := s.principal(r.Context())
	own := sb.CreatedBy == p.Subject
	if !s.authorizeHidden(w, r, project, auth.PermSandboxTerminate, own) {
		return
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	hash := canonicalHash(r.Method, r.URL.Path, raw)
	res, err := s.store.TerminateSandbox(r.Context(), project, sandboxID, p.Subject, key, hash, rid)
	if err != nil {
		writeStoreError(w, rid, err)
		return
	}
	if res.Replay != nil {
		writeReplay(w, res.Replay)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"sandbox": res.Sandbox, "operation": res.Operation})
}

func (s *Server) watchSandbox(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if !s.authorize(w, r, project, auth.PermSandboxGet, false) {
		return
	}
	sandboxID := r.PathValue("sandbox")
	wake, stop := s.watchWake("sandboxes", sandboxID)
	defer stop()
	writeSSE(w, r, sseOpts{
		requestID: s.requestID(r.Context()),
		fetch:     func() (any, error) { return s.store.GetSandbox(r.Context(), project, sandboxID) },
		terminal: func(v any) bool {
			sb := v.(store.Sandbox)
			return sb.State == "FINISHED" || sb.State == "FAILED"
		},
		wake: wake,
	})
}

// watchWake subscribes to store change notifications (when the store supports
// them) and returns a channel that fires when the named resource changes, plus
// a cleanup func. When the store has no notifier, wake is nil and writeSSE
// falls back to a fast poll.
func (s *Server) watchWake(kind, id string) (<-chan struct{}, func()) {
	n, ok := s.store.(store.ChangeNotifier)
	if !ok {
		return nil, func() {}
	}
	changes, cancel := n.Subscribe()
	wake := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case c, open := <-changes:
				if !open {
					return
				}
				if c.Kind == kind && c.ID == id {
					select {
					case wake <- struct{}{}:
					default:
					}
				}
			}
		}
	}()
	return wake, func() { close(done); cancel() }
}

type sseOpts struct {
	requestID string
	fetch     func() (any, error)
	terminal  func(any) bool
	// wake, when non-nil, triggers an immediate re-fetch; the poll interval
	// then only serves as a slow backstop.
	wake <-chan struct{}
	// poll and maxAge default to production values when zero; tests set them
	// short.
	poll   time.Duration
	maxAge time.Duration
}

// writeSSE emits a new snapshot whenever fetch observes a different resource.
// Event IDs are content-derived and therefore stable across API replicas and
// reconnects. A matching Last-Event-ID suppresses replay of the same snapshot.
// The stream stays open until the resource is terminal, the client disconnects,
// or maxAge elapses — it is no longer force-closed after a minute.
func writeSSE(w http.ResponseWriter, r *http.Request, o sseOpts) {
	poll := o.poll
	if poll <= 0 {
		if o.wake != nil {
			poll = 10 * time.Second // backstop only; changes arrive via wake
		} else {
			poll = time.Second
		}
	}
	maxAge := o.maxAge
	if maxAge <= 0 {
		maxAge = 30 * time.Minute
	}

	v, err := o.fetch()
	if err != nil {
		writeStoreError(w, o.requestID, err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	write := func(s string) { _, _ = w.Write([]byte(s)) }
	flush := func() {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	lastID := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	emit := func(snapshot any) (string, error) {
		data, err := json.Marshal(snapshot)
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(data)
		id := fmt.Sprintf("%x", sum[:])
		if id != lastID {
			write("id: " + id + "\nevent: snapshot\ndata: " + string(data) + "\n\n")
			flush()
		}
		return id, nil
	}
	lastID, err = emit(v)
	if err != nil || o.terminal(v) {
		return
	}
	// Ensure a resumed stream is established even when its current snapshot was
	// already received and therefore suppressed.
	write(": connected\n\n")
	flush()

	refresh := func() bool {
		v, err := o.fetch()
		if err != nil {
			return true
		}
		id, err := emit(v)
		if err != nil {
			return true
		}
		lastID = id
		return o.terminal(v)
	}

	pollT := time.NewTicker(poll)
	defer pollT.Stop()
	beat := time.NewTicker(15 * time.Second)
	defer beat.Stop()
	deadline := time.NewTimer(maxAge)
	defer deadline.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			return
		case <-o.wake:
			if refresh() {
				return
			}
		case <-pollT.C:
			if refresh() {
				return
			}
		case <-beat.C:
			write(": heartbeat\n\n")
			flush()
		}
	}
}
