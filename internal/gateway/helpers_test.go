package gateway_test

import (
	"os"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"ignition.dev/ignition/internal/execframe"
)

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

// readData reads frames until it sees a data frame on channel ch, returning its
// payload as a string.
func readData(t *testing.T, conn *websocket.Conn, ch string) string {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		var f execframe.Frame
		if err := conn.ReadJSON(&f); err != nil {
			t.Fatalf("read: %v", err)
		}
		if f.Channel == ch && f.Kind == execframe.Data {
			return string(f.Payload)
		}
		if f.Kind == execframe.Exit {
			t.Fatalf("process exited before data on %s", ch)
		}
	}
}

func readExit(t *testing.T, conn *websocket.Conn) int {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		var f execframe.Frame
		if err := conn.ReadJSON(&f); err != nil {
			t.Fatalf("read: %v", err)
		}
		if f.Kind == execframe.Exit {
			if f.ExitCode == nil {
				return -1
			}
			return *f.ExitCode
		}
	}
}
