package sandboxinit

import (
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"ignition.dev/ignition/internal/execframe"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  32 << 10,
	WriteBufferSize: 32 << 10,
	// The only client is ignition-gateway on the Pod network; it is not a
	// browser, so no Origin check is meaningful here.
	CheckOrigin: func(*http.Request) bool { return true },
}

const (
	writeWait  = 10 * time.Second
	pingPeriod = 25 * time.Second
)

// attach upgrades to a WebSocket and proxies one process's stdio. It trusts the
// caller: ignition-gateway has already validated the exec-stream token. There
// is no tenant-facing route to this endpoint (NetworkPolicy + no Ingress).
func (s *Supervisor) attach(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("process")
	if s.procs == nil {
		http.Error(w, "supervisor disabled", http.StatusServiceUnavailable)
		return
	}
	p, ok := s.procs.lookup(id)
	if !ok {
		http.Error(w, "no such process", http.StatusNotFound)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	// Reader: client stdin and control frames.
	go func() {
		for {
			var f execframe.Frame
			if err := conn.ReadJSON(&f); err != nil {
				return
			}
			if f.Channel != execframe.Stdin {
				continue
			}
			switch f.Kind {
			case execframe.Data:
				if p.stdin != nil && len(f.Payload) > 0 {
					_, _ = p.stdin.Write(f.Payload)
				}
			case execframe.EOF:
				if p.stdin != nil {
					_ = p.stdin.Close()
				}
			}
		}
	}()

	// Keepalive ping.
	stopPing := make(chan struct{})
	defer close(stopPing)
	go func() {
		t := time.NewTicker(pingPeriod)
		defer t.Stop()
		for {
			select {
			case <-stopPing:
				return
			case <-t.C:
				_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeWait))
			}
		}
	}()

	// Writer: process output, then a terminal Exit frame.
	writeErr := p.io.stream(0, func(c ioChunk) error {
		_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
		return conn.WriteJSON(execframe.Frame{Channel: c.Channel, Kind: execframe.Data, ProcessID: id, Payload: c.Data})
	})
	if writeErr != nil {
		return
	}

	m := s.procs
	m.mu.Lock()
	code := p.exitCode
	sig := p.signal
	m.mu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
	_ = conn.WriteJSON(execframe.Frame{
		Channel: execframe.Control, Kind: execframe.Exit, ProcessID: id,
		ExitCode: code, Signal: sig,
	})
	_ = conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(writeWait))
}
