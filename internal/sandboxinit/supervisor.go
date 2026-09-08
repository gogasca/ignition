package sandboxinit

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
)

// DefaultDesiredFile is where the controller's `ignition.io/process-desired`
// annotation is projected by a downwardAPI volume. It holds one JSON object:
// {processID: {command, workingDirectory, environment, pty, signal, cancel}}.
const DefaultDesiredFile = "/etc/ignition/pod/process-desired"

// DefaultWorkRoot is the base for per-process scratch (cwd + captured output).
const DefaultWorkRoot = "/scratch/.ignition/proc"

// desired mirrors the controller's process-desired annotation entry.
type desired struct {
	Command          []string          `json:"command,omitempty"`
	WorkingDirectory string            `json:"workingDirectory,omitempty"`
	Environment      map[string]string `json:"environment,omitempty"`
	PTY              bool              `json:"pty,omitempty"`
	Signal           string            `json:"signal,omitempty"`
	Cancel           bool              `json:"cancel,omitempty"`
}

// observed is what the controller reads back from GET /v1/processes and mirrors
// into the `ignition.io/process-observed` annotation. Keep the JSON shape in
// sync with controller.processObservedRec.
type observed struct {
	State    string `json:"state"`
	ExitCode *int   `json:"exitCode,omitempty"`
	Signal   string `json:"signal,omitempty"`
}

type procState struct {
	id       string
	cmd      *exec.Cmd
	state    string // STARTING, RUNNING, EXITED, FAILED
	exitCode *int
	signal   string
	sigSent  string // last signal name delivered from desired
	canceled bool
	done     chan struct{}
	io       *procIO
	stdin    io.WriteCloser
}

// lookup returns the tracked process for id, if any.
func (m *ProcessManager) lookup(id string) (*procState, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.proc[id]
	return p, ok
}

// ProcessManager runs and tracks the tenant processes the control plane asks
// for. It never talks to Kubernetes: desired state arrives as a projected file,
// observed state leaves over the local HTTP listener.
type ProcessManager struct {
	desiredFile string
	workRoot    string
	gracePeriod time.Duration
	now         func() time.Time

	mu   sync.Mutex
	proc map[string]*procState
	// idle tracking: the sandbox is "active" while any process is
	// STARTING/RUNNING or any exec stream is attached. lastActiveAt is the
	// instant the sandbox last stopped being active (or the manager's start
	// time before anything ran). attachN counts live attach WebSockets.
	lastActiveAt time.Time
	attachN      int
}

func NewProcessManager(desiredFile, workRoot string) *ProcessManager {
	if desiredFile == "" {
		desiredFile = DefaultDesiredFile
	}
	if workRoot == "" {
		workRoot = DefaultWorkRoot
	}
	return &ProcessManager{
		desiredFile:  desiredFile,
		workRoot:     workRoot,
		gracePeriod:  10 * time.Second,
		now:          time.Now,
		proc:         map[string]*procState{},
		lastActiveAt: time.Now(),
	}
}

// attachBegin/attachEnd bracket a live exec stream so an attached-but-quiet
// session still counts as activity.
func (m *ProcessManager) attachBegin() {
	m.mu.Lock()
	m.attachN++
	m.mu.Unlock()
}

func (m *ProcessManager) attachEnd() {
	m.mu.Lock()
	if m.attachN > 0 {
		m.attachN--
	}
	m.lastActiveAt = m.now()
	m.mu.Unlock()
}

// IdleSeconds reports how long the sandbox has had no running process and no
// attached exec stream. It is 0 while the sandbox is active. The controller
// compares this against timeouts.idleSeconds and terminates the sandbox when it
// is exceeded.
func (m *ProcessManager) IdleSeconds() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.attachN > 0 {
		return 0
	}
	for _, p := range m.proc {
		if p.state == "STARTING" || p.state == "RUNNING" {
			return 0
		}
	}
	base := m.lastActiveAt
	if base.IsZero() {
		return 0
	}
	d := m.now().Sub(base)
	if d < 0 {
		return 0
	}
	return int(d.Seconds())
}

// Observed returns the current observed state of every process the manager
// knows about, keyed by process ID.
func (m *ProcessManager) Observed() map[string]observed {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]observed, len(m.proc))
	for id, p := range m.proc {
		out[id] = observed{State: p.state, ExitCode: p.exitCode, Signal: p.signal}
	}
	return out
}

// reconcileOnce reads the desired file and converges running processes to it.
func (m *ProcessManager) reconcileOnce() {
	raw, err := os.ReadFile(m.desiredFile)
	if err != nil {
		return // no desired state yet
	}
	var want map[string]desired
	if err := json.Unmarshal(raw, &want); err != nil {
		return
	}
	// Deterministic order keeps logs and tests stable.
	ids := make([]string, 0, len(want))
	for id := range want {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		m.apply(id, want[id])
	}
}

func (m *ProcessManager) apply(id string, d desired) {
	m.mu.Lock()
	p, known := m.proc[id]
	m.mu.Unlock()

	if !known {
		if len(d.Command) == 0 {
			return
		}
		m.start(id, d)
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if p.state == "EXITED" || p.state == "FAILED" || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	if d.Cancel && !p.canceled {
		p.canceled = true
		go m.cancel(p)
		return
	}
	if d.Signal != "" && d.Signal != p.sigSent {
		p.sigSent = d.Signal
		if sig := unixSignal(d.Signal); sig != 0 {
			_ = p.cmd.Process.Signal(sig)
		}
	}
}

func (m *ProcessManager) start(id string, d desired) {
	dir := filepath.Join(m.workRoot, id)
	_ = os.MkdirAll(dir, 0o755)

	cwd := d.WorkingDirectory
	if cwd == "" {
		cwd = dir
	}
	cmd := exec.Command(d.Command[0], d.Command[1:]...)
	cmd.Dir = cwd
	cmd.Env = flattenEnv(d.Environment)

	pio := newProcIO()
	// stdout/stderr go to both a scratch file (for polling clients) and the
	// live fan-out consumed by the attach WebSocket.
	stdoutW := []io.Writer{pio.writer(ChanStdout)}
	stderrW := []io.Writer{pio.writer(ChanStderr)}
	if f, err := os.Create(filepath.Join(dir, "stdout")); err == nil {
		stdoutW = append(stdoutW, f)
	}
	if f, err := os.Create(filepath.Join(dir, "stderr")); err == nil {
		stderrW = append(stderrW, f)
	}
	cmd.Stdout = io.MultiWriter(stdoutW...)
	cmd.Stderr = io.MultiWriter(stderrW...)
	stdin, _ := cmd.StdinPipe()

	// New process group so cancel can signal the whole tree.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	p := &procState{id: id, cmd: cmd, state: "STARTING", done: make(chan struct{}), io: pio, stdin: stdin}
	m.mu.Lock()
	m.proc[id] = p
	m.mu.Unlock()

	if err := cmd.Start(); err != nil {
		m.mu.Lock()
		p.state = "FAILED"
		m.lastActiveAt = m.now()
		close(p.done)
		m.mu.Unlock()
		pio.close()
		return
	}
	m.mu.Lock()
	p.state = "RUNNING"
	m.mu.Unlock()

	go m.wait(p)
}

func (m *ProcessManager) wait(p *procState) {
	err := p.cmd.Wait()
	m.mu.Lock()
	p.state = "EXITED"
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
			if ws.Signaled() {
				p.signal = signalName(ws.Signal())
				code = 128 + int(ws.Signal())
			} else {
				code = ws.ExitStatus()
			}
		} else {
			code = 1
		}
	} else if err != nil {
		code = 1
	}
	p.exitCode = &code
	m.lastActiveAt = m.now()
	pio := p.io
	stdin := p.stdin
	close(p.done)
	m.mu.Unlock()

	if stdin != nil {
		_ = stdin.Close()
	}
	if pio != nil {
		pio.close()
	}
}

// cancel does graceful SIGTERM to the process group, then SIGKILL after grace.
func (m *ProcessManager) cancel(p *procState) {
	pgid := 0
	m.mu.Lock()
	if p.cmd != nil && p.cmd.Process != nil {
		pgid = p.cmd.Process.Pid
	}
	m.mu.Unlock()
	if pgid == 0 {
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	select {
	case <-p.done:
		return
	case <-time.After(m.gracePeriod):
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
}

// Run drives the reconcile loop until stop is closed.
func (m *ProcessManager) Run(stop <-chan struct{}, interval time.Duration) {
	if interval <= 0 {
		interval = time.Second
	}
	m.reconcileOnce()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			m.reconcileOnce()
		}
	}
}

func flattenEnv(env map[string]string) []string {
	if env == nil {
		return os.Environ()
	}
	out := os.Environ()
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}
