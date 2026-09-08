package probe

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"ignition.dev/ignition/internal/execframe"
)

// streamResult is what streamAttach observed on one attach.
type streamResult struct {
	SawStdout bool
	SawExit   bool // a control/exit frame arrived
	Stdout    string
	ExitCode  *int
}

// streamAttach dials ignition-gateway with a minted stream token, reads output
// frames until the process exits or the peer closes, and reports what it saw. It
// writes no stdin. A transport-level failure (bad handshake, unexpected close)
// is an error; a clean close after an exit frame is not.
func streamAttach(ctx context.Context, gatewayURL, token string) (streamResult, error) {
	u, err := url.Parse(gatewayURL)
	if err != nil {
		return streamResult{}, fmt.Errorf("bad gatewayUrl %q: %w", gatewayURL, err)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "ws", "wss":
	default:
		return streamResult{}, fmt.Errorf("unsupported gatewayUrl scheme %q", u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/attach"
	q := u.Query()
	q.Set("token", token)
	u.RawQuery = q.Encode()

	dialer := &websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	conn, resp, err := dialer.DialContext(ctx, u.String(), http.Header{})
	if err != nil {
		if resp != nil {
			return streamResult{}, fmt.Errorf("gateway attach handshake: %s", resp.Status)
		}
		return streamResult{}, fmt.Errorf("gateway attach handshake: %w", err)
	}
	defer conn.Close()

	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetReadDeadline(dl)
	}

	var res streamResult
	var sb strings.Builder
	for {
		var f execframe.Frame
		if err := conn.ReadJSON(&f); err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				res.Stdout = sb.String()
				return res, nil
			}
			return res, fmt.Errorf("read frame: %w", err)
		}
		switch {
		case f.Channel == execframe.Stdout && f.Kind == execframe.Data:
			res.SawStdout = true
			sb.Write(f.Payload)
		case f.Channel == execframe.Control && f.Kind == execframe.Exit:
			res.SawExit = true
			res.ExitCode = f.ExitCode
			res.Stdout = sb.String()
			return res, nil
		case f.Kind == execframe.Error:
			return res, fmt.Errorf("stream error frame: %s", f.Reason)
		}
	}
}
