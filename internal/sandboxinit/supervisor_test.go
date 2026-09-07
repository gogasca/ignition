package sandboxinit

import (
	"encoding/json"
	"os"
	"path/filepath"
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
