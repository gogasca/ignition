package sandboxinit

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeDesired(t *testing.T, path string, want map[string]desired) {
	t.Helper()
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func newManager(t *testing.T) (*ProcessManager, string) {
	t.Helper()
	dir := t.TempDir()
	desiredFile := filepath.Join(dir, "process-desired")
	if err := os.WriteFile(desiredFile, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	pm := NewProcessManager(desiredFile, filepath.Join(dir, "work"))
	pm.gracePeriod = 200 * time.Millisecond
	return pm, desiredFile
}

func waitFor(t *testing.T, pm *ProcessManager, id string, want string) observed {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		pm.reconcileOnce()
		if o, ok := pm.Observed()[id]; ok && o.State == want {
			return o
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %s never reached %s (have %+v)", id, want, pm.Observed())
	return observed{}
}

func TestSupervisorRunsProcessToExit(t *testing.T) {
	pm, df := newManager(t)
	writeDesired(t, df, map[string]desired{
		"prc_ok": {Command: []string{"sh", "-c", "exit 0"}},
	})
	o := waitFor(t, pm, "prc_ok", "EXITED")
	if o.ExitCode == nil || *o.ExitCode != 0 {
		t.Fatalf("exit code = %v", o.ExitCode)
	}
}

func TestSupervisorCapturesNonZeroExit(t *testing.T) {
	pm, df := newManager(t)
	writeDesired(t, df, map[string]desired{
		"prc_fail": {Command: []string{"sh", "-c", "exit 7"}},
	})
	o := waitFor(t, pm, "prc_fail", "EXITED")
	if o.ExitCode == nil || *o.ExitCode != 7 {
		t.Fatalf("exit code = %v, want 7", o.ExitCode)
	}
}

func TestSupervisorCancelTerminates(t *testing.T) {
	pm, df := newManager(t)
	writeDesired(t, df, map[string]desired{
		"prc_sleep": {Command: []string{"sleep", "60"}},
	})
	waitFor(t, pm, "prc_sleep", "RUNNING")

	writeDesired(t, df, map[string]desired{
		"prc_sleep": {Command: []string{"sleep", "60"}, Cancel: true},
	})
	o := waitFor(t, pm, "prc_sleep", "EXITED")
	// Killed by SIGTERM => 128+15, or SIGKILL => 128+9 if it ignored TERM.
	if o.ExitCode == nil || (*o.ExitCode != 143 && *o.ExitCode != 137) {
		t.Fatalf("cancel exit code = %v", o.ExitCode)
	}
}

func TestSupervisorSignalDelivered(t *testing.T) {
	pm, df := newManager(t)
	cmd := []string{"sleep", "60"}
	writeDesired(t, df, map[string]desired{"prc_sig": {Command: cmd}})
	waitFor(t, pm, "prc_sig", "RUNNING")

	writeDesired(t, df, map[string]desired{"prc_sig": {Command: cmd, Signal: "SIGKILL"}})
	o := waitFor(t, pm, "prc_sig", "EXITED")
	if o.ExitCode == nil || *o.ExitCode != 137 {
		t.Fatalf("signal exit code = %v, want 137", ptrVal(o.ExitCode))
	}
	if o.Signal != "SIGKILL" {
		t.Fatalf("terminating signal = %q, want SIGKILL", o.Signal)
	}
}

func ptrVal(p *int) int {
	if p == nil {
		return -1
	}
	return *p
}

func TestIdleSecondsTracksActivity(t *testing.T) {
	pm, df := newManager(t)
	clock := time.Unix(1_000_000, 0)
	pm.now = func() time.Time { return clock }
	pm.lastActiveAt = clock

	// Fresh manager: idle clock runs from start.
	clock = clock.Add(30 * time.Second)
	if got := pm.IdleSeconds(); got != 30 {
		t.Fatalf("idle before any process = %d, want 30", got)
	}

	// A running process is never idle.
	writeDesired(t, df, map[string]desired{
		"prc_run": {Command: []string{"sh", "-c", "sleep 1"}},
	})
	waitFor(t, pm, "prc_run", "RUNNING")
	clock = clock.Add(time.Hour)
	if got := pm.IdleSeconds(); got != 0 {
		t.Fatalf("idle while process running = %d, want 0", got)
	}

	// After it exits, the idle clock restarts from the exit instant.
	waitFor(t, pm, "prc_run", "EXITED")
	clock = clock.Add(45 * time.Second)
	if got := pm.IdleSeconds(); got != 45 {
		t.Fatalf("idle after process exit = %d, want 45", got)
	}

	// A live attach counts as activity even with no process.
	pm.attachBegin()
	clock = clock.Add(time.Hour)
	if got := pm.IdleSeconds(); got != 0 {
		t.Fatalf("idle with attach open = %d, want 0", got)
	}
	pm.attachEnd()
	clock = clock.Add(10 * time.Second)
	if got := pm.IdleSeconds(); got != 10 {
		t.Fatalf("idle after attach closed = %d, want 10", got)
	}
}

func TestSupervisorAllocatesPTY(t *testing.T) {
	pm, df := newManager(t)
	// `test -t 1` exits 0 only when stdout is a terminal; `tput`-free size
	// check via stty confirms the winsize was applied.
	writeDesired(t, df, map[string]desired{
		"prc_pty": {
			Command: []string{"sh", "-c", "test -t 1 && stty size"},
			PTY:     true,
			PTYRows: 40,
			PTYCols: 100,
		},
	})
	o := waitFor(t, pm, "prc_pty", "EXITED")
	if o.ExitCode == nil || *o.ExitCode != 0 {
		t.Fatalf("process on a PTY exited %v, want 0 (stdout was not a tty)", o.ExitCode)
	}

	// Drain the merged output; it must report the 40x100 window.
	var buf []byte
	pm.mu.Lock()
	p := pm.proc["prc_pty"]
	pm.mu.Unlock()
	_ = p.io.stream(0, func(c ioChunk) error {
		buf = append(buf, c.Data...)
		return nil
	})
	if got := strings.TrimSpace(string(buf)); got != "40 100" {
		t.Fatalf("stty size = %q, want \"40 100\"", got)
	}
}

func TestSupervisorPTYStdinEcho(t *testing.T) {
	pm, df := newManager(t)
	writeDesired(t, df, map[string]desired{
		"prc_cat": {Command: []string{"cat"}, PTY: true},
	})
	waitFor(t, pm, "prc_cat", "RUNNING")
	pm.mu.Lock()
	p := pm.proc["prc_cat"]
	pm.mu.Unlock()

	got := make(chan string, 1)
	go func() {
		var buf []byte
		_ = p.io.stream(0, func(c ioChunk) error {
			buf = append(buf, c.Data...)
			if strings.Contains(string(buf), "ping") {
				got <- string(buf)
				return io.EOF
			}
			return nil
		})
	}()
	_, _ = p.stdin.Write([]byte("ping\r"))
	select {
	case out := <-got:
		if !strings.Contains(out, "ping") {
			t.Fatalf("pty echo = %q", out)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no PTY echo within 3s")
	}
	_ = p.stdin.Close()
}

func TestSupervisorIgnoresRemovedDesired(t *testing.T) {
	pm, df := newManager(t)
	writeDesired(t, df, map[string]desired{
		"prc_a": {Command: []string{"sh", "-c", "exit 0"}},
	})
	waitFor(t, pm, "prc_a", "EXITED")
	// Removing it from desired must not resurrect or drop the observed record.
	writeDesired(t, df, map[string]desired{})
	pm.reconcileOnce()
	if _, ok := pm.Observed()["prc_a"]; !ok {
		t.Fatal("observed record disappeared after desired removal")
	}
}
