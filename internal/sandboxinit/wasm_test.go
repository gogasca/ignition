package sandboxinit

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tetratelabs/wazero"
)

var (
	// testWASICache lets every test reuse the one compile of the probe.
	testWASICache = wazero.NewCompilationCache()

	probeOnce sync.Once
	probePath string
	probeErr  error
)

// wasiProbe builds testdata/wasiprobe for wasip1 once per test binary.
func wasiProbe(t *testing.T) string {
	t.Helper()
	probeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "wasiprobe")
		if err != nil {
			probeErr = err
			return
		}
		probePath = filepath.Join(dir, "wasiprobe.wasm")
		cmd := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", probePath, "./testdata/wasiprobe")
		cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			probeErr = err
			probePath = string(out)
		}
	})
	if probeErr != nil {
		t.Fatalf("build wasiprobe: %v\n%s", probeErr, probePath)
	}
	return probePath
}

// waitForSlow is waitFor with room for the first compile of the probe.
func waitForSlow(t *testing.T, pm *ProcessManager, id, want string) observed {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
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

func newWASIManager(t *testing.T) (*ProcessManager, string) {
	t.Helper()
	pm, df := newManager(t)
	pm.wasiCache = testWASICache
	return pm, df
}

func wasiCmd(t *testing.T, args ...string) []string {
	return append([]string{wasiProbe(t)}, args...)
}

func readOut(t *testing.T, pm *ProcessManager, id, stream string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(pm.workRoot, id, stream))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWASIRunsModuleWithArgs(t *testing.T) {
	pm, df := newWASIManager(t)
	writeDesired(t, df, map[string]desired{
		"prc_w": {Command: wasiCmd(t, "echo", "hello", "wasm"), Runtime: RuntimeWASI},
	})
	o := waitForSlow(t, pm, "prc_w", "EXITED")
	if o.ExitCode == nil || *o.ExitCode != 0 {
		t.Fatalf("exit code = %v, stderr=%q", o.ExitCode, readOut(t, pm, "prc_w", "stderr"))
	}
	if got := readOut(t, pm, "prc_w", "stdout"); got != "hello wasm\n" {
		t.Fatalf("stdout = %q", got)
	}
}

func TestWASIExitCode(t *testing.T) {
	pm, df := newWASIManager(t)
	writeDesired(t, df, map[string]desired{
		"prc_w": {Command: wasiCmd(t, "exit", "7"), Runtime: RuntimeWASI},
	})
	o := waitForSlow(t, pm, "prc_w", "EXITED")
	if o.ExitCode == nil || *o.ExitCode != 7 {
		t.Fatalf("exit code = %v, want 7", o.ExitCode)
	}
}

func TestWASIEnvironmentIsOnlyTheRequested(t *testing.T) {
	t.Setenv("SUPERVISOR_ONLY", "leak")
	pm, df := newWASIManager(t)
	writeDesired(t, df, map[string]desired{
		"prc_w": {Command: wasiCmd(t, "environ"), Runtime: RuntimeWASI, Environment: map[string]string{"TASK": "t1"}},
	})
	waitForSlow(t, pm, "prc_w", "EXITED")
	got := readOut(t, pm, "prc_w", "stdout")
	if !strings.Contains(got, "TASK=t1") || strings.Contains(got, "SUPERVISOR_ONLY") {
		t.Fatalf("environ = %q", got)
	}
}

func TestWASIFilesystemIsWorkingDirectory(t *testing.T) {
	pm, df := newWASIManager(t)
	wd := t.TempDir()
	if err := os.WriteFile(filepath.Join(wd, "in.txt"), []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeDesired(t, df, map[string]desired{
		"prc_r": {Command: wasiCmd(t, "cat", "in.txt"), Runtime: RuntimeWASI, WorkingDirectory: wd},
		"prc_w": {Command: wasiCmd(t, "write", "out.txt", "result"), Runtime: RuntimeWASI, WorkingDirectory: wd},
		"prc_x": {Command: wasiCmd(t, "cat", "/etc/passwd"), Runtime: RuntimeWASI, WorkingDirectory: wd},
	})
	waitForSlow(t, pm, "prc_r", "EXITED")
	if got := readOut(t, pm, "prc_r", "stdout"); got != "fixture" {
		t.Fatalf("read = %q", got)
	}
	waitForSlow(t, pm, "prc_w", "EXITED")
	if b, err := os.ReadFile(filepath.Join(wd, "out.txt")); err != nil || string(b) != "result" {
		t.Fatalf("write = %q, %v", b, err)
	}
	// Nothing outside the working directory is visible to the module.
	o := waitForSlow(t, pm, "prc_x", "EXITED")
	if o.ExitCode == nil || *o.ExitCode != 1 {
		t.Fatalf("host file read exit = %v, stdout=%q", o.ExitCode, readOut(t, pm, "prc_x", "stdout"))
	}
}

func TestWASIStdin(t *testing.T) {
	pm, df := newWASIManager(t)
	writeDesired(t, df, map[string]desired{
		"prc_w": {Command: wasiCmd(t, "stdin"), Runtime: RuntimeWASI},
	})
	waitForSlow(t, pm, "prc_w", "RUNNING")
	p, _ := pm.lookup("prc_w")
	if _, err := p.stdin.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	_ = p.stdin.Close()
	waitForSlow(t, pm, "prc_w", "EXITED")
	if got := readOut(t, pm, "prc_w", "stdout"); got != "ABC" {
		t.Fatalf("stdout = %q", got)
	}
}

func TestWASICancelStopsSpinningModule(t *testing.T) {
	pm, df := newWASIManager(t)
	cmd := wasiCmd(t, "spin")
	writeDesired(t, df, map[string]desired{"prc_w": {Command: cmd, Runtime: RuntimeWASI}})
	waitForSlow(t, pm, "prc_w", "RUNNING")
	if pm.IdleSeconds() != 0 {
		t.Fatal("a running module must count as activity")
	}
	writeDesired(t, df, map[string]desired{"prc_w": {Command: cmd, Runtime: RuntimeWASI, Cancel: true}})
	o := waitForSlow(t, pm, "prc_w", "EXITED")
	if o.ExitCode == nil || *o.ExitCode != 137 || o.Signal != "SIGKILL" {
		t.Fatalf("cancel = %+v", o)
	}
}

func TestWASISignalTerminates(t *testing.T) {
	pm, df := newWASIManager(t)
	cmd := wasiCmd(t, "stdin") // parked on a stdin read, not spinning
	writeDesired(t, df, map[string]desired{"prc_w": {Command: cmd, Runtime: RuntimeWASI}})
	waitForSlow(t, pm, "prc_w", "RUNNING")
	writeDesired(t, df, map[string]desired{"prc_w": {Command: cmd, Runtime: RuntimeWASI, Signal: "SIGTERM"}})
	o := waitForSlow(t, pm, "prc_w", "EXITED")
	if o.ExitCode == nil || *o.ExitCode != 143 || o.Signal != "SIGTERM" {
		t.Fatalf("signal = %+v", o)
	}
}

func TestWASIMemoryLimitTraps(t *testing.T) {
	pm, df := newWASIManager(t)
	pm.wasiMemoryMiB = 64
	writeDesired(t, df, map[string]desired{
		"prc_w": {Command: wasiCmd(t, "alloc"), Runtime: RuntimeWASI},
	})
	o := waitForSlow(t, pm, "prc_w", "EXITED")
	if o.ExitCode == nil || *o.ExitCode == 0 {
		t.Fatalf("over-limit module exit = %v", o.ExitCode)
	}
}

func TestWASIMissingModuleFails(t *testing.T) {
	pm, df := newWASIManager(t)
	writeDesired(t, df, map[string]desired{
		"prc_w": {Command: []string{"/nope/missing.wasm"}, Runtime: RuntimeWASI},
	})
	waitForSlow(t, pm, "prc_w", "FAILED")
	if got := readOut(t, pm, "prc_w", "stderr"); !strings.Contains(got, "missing.wasm") {
		t.Fatalf("stderr = %q", got)
	}
}

func TestWASIRejectsNonWasmFile(t *testing.T) {
	pm, df := newWASIManager(t)
	bogus := filepath.Join(t.TempDir(), "x.wasm")
	if err := os.WriteFile(bogus, []byte("#!/bin/sh\necho pwned\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeDesired(t, df, map[string]desired{"prc_w": {Command: []string{bogus}, Runtime: RuntimeWASI}})
	waitForSlow(t, pm, "prc_w", "FAILED")
	if got := readOut(t, pm, "prc_w", "stdout"); got != "" {
		t.Fatalf("non-wasm file ran natively: %q", got)
	}
}

func TestUnknownRuntimeFailsClosed(t *testing.T) {
	pm, df := newWASIManager(t)
	writeDesired(t, df, map[string]desired{
		"prc_w": {Command: []string{"sh", "-c", "echo ran"}, Runtime: "FUTURE"},
	})
	waitFor(t, pm, "prc_w", "FAILED")
	if got := readOut(t, pm, "prc_w", "stdout"); got != "" {
		t.Fatalf("unknown runtime ran natively: %q", got)
	}
}

func TestWASIConcurrentLaunchesCompileOnce(t *testing.T) {
	pm, df := newManager(t) // fresh cache: nothing compiled yet
	want := map[string]desired{}
	for _, id := range []string{"prc_a", "prc_b", "prc_c", "prc_d"} {
		want[id] = desired{Command: wasiCmd(t, "exit", "0"), Runtime: RuntimeWASI}
	}
	start := time.Now()
	writeDesired(t, df, map[string]desired{"prc_a": want["prc_a"]})
	waitForSlow(t, pm, "prc_a", "EXITED")
	one := time.Since(start)

	pm2, df2 := newManager(t)
	start = time.Now()
	writeDesired(t, df2, want)
	for id := range want {
		waitForSlow(t, pm2, id, "EXITED")
	}
	four := time.Since(start)
	// Four duplicate compiles would take ~4x one; one compile plus three cache
	// hits stays well under 2x.
	if four > 2*one+time.Second {
		t.Fatalf("4 concurrent launches took %v vs %v for one: module compiled more than once", four, one)
	}
}
