package api

import (
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
	WorkingDirectory string               `json:"workingDirectory"`
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
		ProjectID:  project,
		Principal:  s.principal(r.Context()).Subject,
		IdemKey:    key,
		IdemHash:   hash,
		Name:       in.Name,
		ImageID:    in.ImageID,
		Command:    in.Command,
		WorkingDir: in.WorkingDir,
		Resources:  in.Resources,
		Placement:  in.Placement,
		Timeouts:   in.Timeouts,
		Network:    in.Network,
		Labels:     in.Labels,
		SecretRefs: in.SecretRefs,
		TraceID:    rid,
		MaxActive:  s.cfg.MaxActiveSandboxes,
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

	return store.CreateSandboxInput{
		Name:       body.Name,
		ImageID:    body.ImageID,
		Command:    body.Command,
		WorkingDir: body.WorkingDirectory,
		Resources:  rt.Resources,
		Placement:  rt.Placement,
		Timeouts:   rt.Timeouts,
		Network:    rt.Network,
		Labels:     body.Labels,
		SecretRefs: body.SecretRefs,
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
	writeSSE(w, r, s.requestID(r.Context()), func() (any, error) {
		return s.store.GetSandbox(r.Context(), project, sandboxID)
	}, func(v any) bool {
		sb := v.(store.Sandbox)
		return sb.State == "FINISHED" || sb.State == "FAILED"
	})
}

// writeSSE emits a new snapshot whenever fetch observes a different resource.
// Event IDs are content-derived and therefore stable across API replicas and
// reconnects. A matching Last-Event-ID suppresses replay of the same snapshot.
func writeSSE(w http.ResponseWriter, r *http.Request, requestID string, fetch func() (any, error), terminal func(any) bool) {
	v, err := fetch()
	if err != nil {
		writeStoreError(w, requestID, err)
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
	if err != nil || terminal(v) {
		return
	}
	// Ensure a resumed stream is established even when its current snapshot was
	// already received and therefore suppressed.
	write(": connected\n\n")
	flush()

	poll := time.NewTicker(time.Second)
	defer poll.Stop()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			return
		case <-poll.C:
			v, err := fetch()
			if err != nil {
				return
			}
			id, err := emit(v)
			if err != nil {
				return
			}
			lastID = id
			if terminal(v) {
				return
			}
		case <-ticker.C:
			write(": heartbeat\n\n")
			flush()
		}
	}
}
