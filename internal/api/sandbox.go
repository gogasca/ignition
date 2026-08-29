package api

import (
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
	Environment      map[string]string    `json:"environment"`
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
		ProjectID:   project,
		Principal:   s.principal(r.Context()).Subject,
		IdemKey:     key,
		IdemHash:    hash,
		Name:        in.Name,
		ImageID:     in.ImageID,
		Command:     in.Command,
		WorkingDir:  in.WorkingDir,
		Environment: in.Environment,
		Resources:   in.Resources,
		Placement:   in.Placement,
		Timeouts:    in.Timeouts,
		Network:     in.Network,
		Labels:      in.Labels,
		SecretRefs:  in.SecretRefs,
		TraceID:     rid,
		MaxActive:   s.cfg.MaxActiveSandboxes,
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
	if body.Resources == nil || body.Resources.CPUMilli < 1 || body.Resources.MemoryMiB < 1 {
		return store.CreateSandboxInput{}, fmt.Errorf("resources.cpuMilli and resources.memoryMiB are required")
	}
	if body.Resources.CPUMilli > maxCPUMilli {
		return store.CreateSandboxInput{}, fmt.Errorf("resources.cpuMilli exceeds %d", maxCPUMilli)
	}
	if body.Resources.MemoryMiB > maxMemoryMiB {
		return store.CreateSandboxInput{}, fmt.Errorf("resources.memoryMiB exceeds %d", maxMemoryMiB)
	}
	if body.Resources.GPU.Count != 1 {
		return store.CreateSandboxInput{}, fmt.Errorf("gpu.count must be 1")
	}
	if body.Resources.GPU.Type == "" {
		return store.CreateSandboxInput{}, fmt.Errorf("gpu.type is required")
	}
	if !store.ValidGPUType(body.Resources.GPU.Type) {
		return store.CreateSandboxInput{}, fmt.Errorf("gpu.type %q is not a supported GpuType", body.Resources.GPU.Type)
	}
	if !s.cfg.GPUTypeAllowed(body.Resources.GPU.Type) {
		return store.CreateSandboxInput{}, fmt.Errorf("gpu.type %q is not allowed", body.Resources.GPU.Type)
	}
	if err := checkCommand(body.Command); err != nil {
		return store.CreateSandboxInput{}, err
	}
	if err := checkEnv(body.Environment); err != nil {
		return store.CreateSandboxInput{}, err
	}
	if err := checkLabels(body.Labels); err != nil {
		return store.CreateSandboxInput{}, err
	}
	if err := checkSecretRefs(body.SecretRefs, body.Environment); err != nil {
		return store.CreateSandboxInput{}, err
	}
	region := s.cfg.EnabledRegion
	if region == "" {
		region = "us-central1"
	}
	pref := "ON_DEMAND"
	if body.Placement != nil {
		if body.Placement.Region != "" && body.Placement.Region != region {
			return store.CreateSandboxInput{}, fmt.Errorf("region %q is not enabled", body.Placement.Region)
		}
		if body.Placement.ProvisioningPreference != "" {
			switch body.Placement.ProvisioningPreference {
			case "ON_DEMAND", "SPOT_ALLOWED", "SPOT_ONLY":
				pref = body.Placement.ProvisioningPreference
			default:
				return store.CreateSandboxInput{}, fmt.Errorf("invalid provisioningPreference")
			}
		}
	}
	for k := range body.Labels {
		if strings.HasPrefix(k, "ignition.") {
			return store.CreateSandboxInput{}, fmt.Errorf("label key %q is reserved", k)
		}
	}
	timeouts := store.TimeoutSpec{
		StartupSeconds:          120,
		MaximumRuntimeSeconds:   3600,
		IdleSeconds:             600,
		TerminationGraceSeconds: 20,
	}
	if body.Timeouts != nil {
		if body.Timeouts.StartupSeconds > 0 {
			timeouts.StartupSeconds = body.Timeouts.StartupSeconds
		}
		if body.Timeouts.MaximumRuntimeSeconds > 0 {
			timeouts.MaximumRuntimeSeconds = body.Timeouts.MaximumRuntimeSeconds
		}
		if body.Timeouts.IdleSeconds > 0 {
			timeouts.IdleSeconds = body.Timeouts.IdleSeconds
		}
		if body.Timeouts.TerminationGraceSeconds > 0 {
			timeouts.TerminationGraceSeconds = body.Timeouts.TerminationGraceSeconds
		}
	}
	if err := checkTimeouts(timeouts); err != nil {
		return store.CreateSandboxInput{}, err
	}
	net := store.NetworkSpec{Egress: store.EgressSpec{Mode: "DENY_ALL"}}
	if body.Network != nil && body.Network.Egress.Mode != "" {
		switch body.Network.Egress.Mode {
		case "DENY_ALL":
			net = *body.Network
			net.Egress.Mode = "DENY_ALL"
		case "ALLOW_LIST":
			net = *body.Network
			if err := checkAllowList(net.Egress); err != nil {
				return store.CreateSandboxInput{}, err
			}
		default:
			return store.CreateSandboxInput{}, fmt.Errorf("invalid egress mode")
		}
	}
	return store.CreateSandboxInput{
		Name:        body.Name,
		ImageID:     body.ImageID,
		Command:     body.Command,
		WorkingDir:  body.WorkingDirectory,
		Environment: body.Environment,
		Resources:   *body.Resources,
		Placement: store.PlacementSpec{
			Region:                 region,
			ProvisioningPreference: pref,
		},
		Timeouts:   timeouts,
		Network:    net,
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
	sb, err := s.store.GetSandbox(r.Context(), project, r.PathValue("sandbox"))
	if err != nil {
		writeStoreError(w, s.requestID(r.Context()), err)
		return
	}
	writeSSE(w, r, sb)
}

func writeSSE(w http.ResponseWriter, r *http.Request, v any) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	data, _ := json.Marshal(v)
	write := func(s string) { _, _ = w.Write([]byte(s)) }
	write("id: 1\nevent: snapshot\ndata: " + string(data) + "\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
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
		case <-ticker.C:
			write(": heartbeat\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}
}
