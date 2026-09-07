// Package execframe is the JSON exec-stream frame exchanged over the attach
// WebSocket between ignitionctl, ignition-gateway (which proxies it opaquely),
// and the sandbox-init supervisor. It mirrors ExecFrame in process.proto; the
// proto stays the canonical schema, this is the v1 wire encoding.
//
// encoding/json renders the []byte Payload as base64 automatically, which keeps
// the stream binary-safe.
package execframe

// Channel values.
const (
	Stdin   = "stdin"
	Stdout  = "stdout"
	Stderr  = "stderr"
	Control = "control"
)

// Kind values.
const (
	Data  = "data"
	EOF   = "eof"   // stdin closed by the client
	Exit  = "exit"  // process exited (control channel)
	Error = "error" // fatal stream error
)

// Frame is one message on the attach stream.
type Frame struct {
	Channel   string `json:"channel"`
	Kind      string `json:"kind"`
	ProcessID string `json:"processId,omitempty"`
	Payload   []byte `json:"payload,omitempty"`
	ExitCode  *int   `json:"exitCode,omitempty"`
	Signal    string `json:"signal,omitempty"`
	Reason    string `json:"reason,omitempty"`
}
