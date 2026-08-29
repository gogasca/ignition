package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"ignition.dev/ignition/internal/auth"
	"ignition.dev/ignition/internal/store"
)

type createProcessBody struct {
	Command          []string          `json:"command"`
	WorkingDirectory string            `json:"workingDirectory"`
	Environment      map[string]string `json:"environment"`
	PTY              bool              `json:"pty"`
}

type signalProcessBody struct {
	Signal string `json:"signal"`
}

type attachResponse struct {
	StreamToken string    `json:"streamToken"`
	GatewayURL  string    `json:"gatewayUrl"`
	ExpireTime  time.Time `json:"expireTime"`
	StreamEpoch int64     `json:"streamEpoch"`
}

func (s *Server) createProcess(w http.ResponseWriter, r *http.Request) {
	rid := s.requestID(r.Context())
	project := r.PathValue("project")
	sandboxID := r.PathValue("sandbox")
	if !s.authorize(w, r, project, auth.PermSandboxExec, false) {
		return
	}
	key, ok := requireIdempotency(w, rid, r)
	if !ok {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeStatus(w, rid, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid body", false, 0)
		return
	}
	var body createProcessBody
	if err := json.Unmarshal(raw, &body); err != nil {
		writeStatus(w, rid, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid JSON", false, 0)
		return
	}
	if len(body.Command) == 0 {
		writeStatus(w, rid, http.StatusBadRequest, "INVALID_ARGUMENT", "command is required", false, 0)
		return
	}
	if err := checkCommand(body.Command); err != nil {
		writeStatus(w, rid, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error(), false, 0)
		return
	}
	if err := checkEnv(body.Environment); err != nil {
		writeStatus(w, rid, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error(), false, 0)
		return
	}
	p, replay, err := s.store.CreateProcess(r.Context(), store.CreateProcessInput{
		ProjectID:   project,
		SandboxID:   sandboxID,
		Principal:   s.principal(r.Context()).Subject,
		IdemKey:     key,
		IdemHash:    canonicalHash(r.Method, r.URL.Path, raw),
		Command:     body.Command,
		WorkingDir:  body.WorkingDirectory,
		Environment: body.Environment,
		PTY:         body.PTY,
	})
	if err != nil {
		writeStoreError(w, rid, err)
		return
	}
	if replay != nil {
		writeReplay(w, replay)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) getProcess(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if !s.authorize(w, r, project, auth.PermProcessGet, false) {
		return
	}
	p, err := s.store.GetProcess(r.Context(), project, r.PathValue("sandbox"), r.PathValue("process"))
	if err != nil {
		writeStoreError(w, s.requestID(r.Context()), err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) listProcesses(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if !s.authorize(w, r, project, auth.PermProcessGet, false) {
		return
	}
	size, _ := strconv.Atoi(r.URL.Query().Get("pageSize"))
	items, next, err := s.store.ListProcesses(r.Context(), project, r.PathValue("sandbox"), size, r.URL.Query().Get("pageToken"))
	if err != nil {
		writeStoreError(w, s.requestID(r.Context()), err)
		return
	}
	if items == nil {
		items = []store.Process{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"processes": items, "nextPageToken": next})
}

func (s *Server) attachProcess(w http.ResponseWriter, r *http.Request) {
	rid := s.requestID(r.Context())
	project := r.PathValue("project")
	sandboxID := r.PathValue("sandbox")
	processID := r.PathValue("process")
	if !s.authorize(w, r, project, auth.PermSandboxExec, false) {
		return
	}
	key, ok := requireIdempotency(w, rid, r)
	if !ok {
		return
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	hash := canonicalHash(r.Method, r.URL.Path, raw)
	sb, err := s.store.GetSandbox(r.Context(), project, sandboxID)
	if err != nil {
		writeStoreError(w, rid, err)
		return
	}
	if sb.State != "READY" {
		writeStatus(w, rid, http.StatusBadRequest, "FAILED_PRECONDITION", "sandbox is not READY", false, 0)
		return
	}
	proc, err := s.store.GetProcess(r.Context(), project, sandboxID, processID)
	if err != nil {
		writeStoreError(w, rid, err)
		return
	}
	replay, err := s.store.Idempotent(r.Context(), store.IdempotentInput{
		Principal: s.principal(r.Context()).Subject,
		ProjectID: project,
		Method:    r.Method,
		Route:     r.URL.Path,
		Key:       key,
		Hash:      hash,
	}, func() (int, []byte, error) {
		now := s.now()
		exp := now.Add(5 * time.Minute)
		epoch := now.UnixNano()
		token, err := s.mintStreamToken(s.principal(r.Context()), sb, proc, epoch, exp)
		if err != nil {
			return 0, nil, err
		}
		gw := s.cfg.GatewayURL
		if gw == "" {
			gw = "https://gateway.us-central1.ignition.dev"
		}
		body, err := json.Marshal(attachResponse{
			StreamToken: token,
			GatewayURL:  gw,
			ExpireTime:  exp.UTC(),
			StreamEpoch: epoch,
		})
		return http.StatusOK, body, err
	})
	if err != nil {
		writeStatus(w, rid, http.StatusInternalServerError, "UNAVAILABLE", "failed to mint stream token", true, 1)
		return
	}
	writeReplay(w, replay)
}

func (s *Server) signalProcess(w http.ResponseWriter, r *http.Request) {
	rid := s.requestID(r.Context())
	project := r.PathValue("project")
	if !s.authorize(w, r, project, auth.PermSandboxExec, false) {
		return
	}
	key, ok := requireIdempotency(w, rid, r)
	if !ok {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		writeStatus(w, rid, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid body", false, 0)
		return
	}
	var body signalProcessBody
	if err := json.Unmarshal(raw, &body); err != nil || body.Signal == "" {
		writeStatus(w, rid, http.StatusBadRequest, "INVALID_ARGUMENT", "signal is required", false, 0)
		return
	}
	if !validSignal(body.Signal) {
		writeStatus(w, rid, http.StatusBadRequest, "INVALID_ARGUMENT", "signal is not allowed", false, 0)
		return
	}
	p, replay, err := s.store.SignalProcess(
		r.Context(),
		project,
		r.PathValue("sandbox"),
		r.PathValue("process"),
		s.principal(r.Context()).Subject,
		key,
		canonicalHash(r.Method, r.URL.Path, raw),
		body.Signal,
	)
	if err != nil {
		writeStoreError(w, rid, err)
		return
	}
	if replay != nil {
		writeReplay(w, replay)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) cancelProcess(w http.ResponseWriter, r *http.Request) {
	rid := s.requestID(r.Context())
	project := r.PathValue("project")
	if !s.authorize(w, r, project, auth.PermSandboxExec, false) {
		return
	}
	key, ok := requireIdempotency(w, rid, r)
	if !ok {
		return
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	p, replay, err := s.store.CancelProcess(
		r.Context(),
		project,
		r.PathValue("sandbox"),
		r.PathValue("process"),
		s.principal(r.Context()).Subject,
		key,
		canonicalHash(r.Method, r.URL.Path, raw),
	)
	if err != nil {
		writeStoreError(w, rid, err)
		return
	}
	if replay != nil {
		writeReplay(w, replay)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func requireIdempotency(w http.ResponseWriter, requestID string, r *http.Request) (string, bool) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		writeStatus(w, requestID, http.StatusBadRequest, "INVALID_ARGUMENT", "Idempotency-Key is required", false, 0)
		return "", false
	}
	if len(key) > maxIdempotencyKey {
		writeStatus(w, requestID, http.StatusBadRequest, "INVALID_ARGUMENT", "Idempotency-Key is too long", false, 0)
		return "", false
	}
	return key, true
}

func (s *Server) mintStreamToken(p auth.Principal, sb store.Sandbox, proc store.Process, epoch int64, exp time.Time) (string, error) {
	secret := s.cfg.StreamTokenSecret
	if secret == "" {
		return "", fmt.Errorf("stream token secret is not configured")
	}
	aud := s.cfg.GatewayURL
	if aud == "" {
		aud = "https://gateway.us-central1.ignition.dev"
	}
	now := s.now()
	return signStreamToken(secret, aud, p.Subject, sb.ProjectID, sb.ID, proc.ID, sb.Generation, epoch, now, exp)
}
