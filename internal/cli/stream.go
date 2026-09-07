package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"ignition.dev/ignition/internal/execframe"
)

// streamExec attaches to a process through ignition-gateway and pumps stdio
// until the process exits. It returns the process exit code (or -1 if unknown).
func streamExec(ctx context.Context, gatewayURL, token, processID string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	u, err := url.Parse(gatewayURL)
	if err != nil {
		return -1, fmt.Errorf("bad gatewayUrl %q: %w", gatewayURL, err)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "ws", "wss":
	default:
		return -1, fmt.Errorf("unsupported gatewayUrl scheme %q", u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/attach"
	q := u.Query()
	q.Set("token", token)
	u.RawQuery = q.Encode()

	dialer := &websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	conn, resp, err := dialer.DialContext(ctx, u.String(), http.Header{})
	if err != nil {
		if resp != nil {
			return -1, fmt.Errorf("gateway attach failed: %s", resp.Status)
		}
		return -1, fmt.Errorf("gateway attach failed: %w", err)
	}
	defer conn.Close()

	// stdin pump
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, rerr := stdin.Read(buf)
			if n > 0 {
				_ = conn.WriteJSON(execframe.Frame{Channel: execframe.Stdin, Kind: execframe.Data, Payload: append([]byte(nil), buf[:n]...)})
			}
			if rerr != nil {
				_ = conn.WriteJSON(execframe.Frame{Channel: execframe.Stdin, Kind: execframe.EOF})
				return
			}
		}
	}()

	for {
		var f execframe.Frame
		if err := conn.ReadJSON(&f); err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				return -1, nil
			}
			return -1, err
		}
		switch {
		case f.Channel == execframe.Stdout && f.Kind == execframe.Data:
			_, _ = stdout.Write(f.Payload)
		case f.Channel == execframe.Stderr && f.Kind == execframe.Data:
			_, _ = stderr.Write(f.Payload)
		case f.Channel == execframe.Control && f.Kind == execframe.Exit:
			if f.ExitCode != nil {
				return *f.ExitCode, nil
			}
			return -1, nil
		case f.Kind == execframe.Error:
			return -1, fmt.Errorf("stream error: %s", f.Reason)
		}
	}
}
