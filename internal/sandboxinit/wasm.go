package sandboxinit

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

// RuntimeWASI marks a desired process whose command[0] is a WebAssembly
// module (WASI preview 1) that sandbox-init runs in its embedded engine
// instead of exec'ing a Linux binary. The empty runtime is a native process.
const RuntimeWASI = "WASI"

const (
	// defaultWASIMemoryMiB caps one module's linear memory. The Pod's memory
	// limit still bounds the sandbox as a whole; this keeps one runaway
	// module from taking all of it.
	defaultWASIMemoryMiB = 1024
	// maxWASIModuleBytes bounds how much of a module file is read into memory
	// before compilation.
	maxWASIModuleBytes = 256 << 20
	wasmPageBytes      = 64 << 10
	// maxWASIMemoryMiB is wasm32's whole 4 GiB address space.
	maxWASIMemoryMiB = 4096
)

// signalCause is the cancellation cause recorded when a signal or cancel
// stops a WASI module. WASI preview 1 has no signal delivery, so every
// allowlisted signal terminates the module, which is each one's default
// action for a native process too.
type signalCause struct{ sig syscall.Signal }

func (c signalCause) Error() string { return "stopped by " + signalName(c.sig) }

// wasmRun is the handle a supervised WASI module exposes to apply/cancel.
type wasmRun struct {
	stop  context.CancelCauseFunc
	stdin *io.PipeWriter
}

// terminate stops the module at its next function boundary or loop back-edge
// and unblocks a pending stdin read, so a module parked on input also exits.
func (w *wasmRun) terminate(sig syscall.Signal) {
	w.stop(signalCause{sig: sig})
	_ = w.stdin.CloseWithError(io.ErrClosedPipe)
}

// startWASI runs d.Command[0] as a WASI module. The working directory is the
// module's only filesystem, mounted at "/"; the environment is exactly
// d.Environment (the supervisor's own env is not inherited); argv is
// d.Command. Compilation happens off the reconcile loop while the process is
// STARTING, and a module that cannot be read or compiled ends FAILED with the
// reason on stderr.
func (m *ProcessManager) startWASI(p *procState, d desired, cwd string, stdout, stderr io.Writer, closers []io.Closer) {
	ctx, stop := context.WithCancelCause(context.Background())
	stdinR, stdinW := io.Pipe()
	run := &wasmRun{stop: stop, stdin: stdinW}
	p.wasm = run
	p.stdin = stdinW

	m.mu.Lock()
	m.proc[p.id] = p
	m.mu.Unlock()

	go func() {
		defer stop(nil)
		defer func() {
			for _, c := range closers {
				_ = c.Close()
			}
		}()
		code, sigName, err := m.runWASI(ctx, p, d, cwd, stdinR, stdout, stderr)
		_ = stdinW.Close()
		m.mu.Lock()
		if err != nil {
			p.state = "FAILED"
		} else {
			p.state = "EXITED"
			p.exitCode = &code
			p.signal = sigName
		}
		m.lastActiveAt = m.now()
		close(p.done)
		m.mu.Unlock()
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "ignition: wasi: %v\n", err)
		}
		p.io.close()
	}()
}

// runWASI compiles and runs one module. err is non-nil only when the module
// never started (unreadable, invalid, or stopped before it ran); a module
// that started always yields an exit code.
func (m *ProcessManager) runWASI(ctx context.Context, p *procState, d desired, cwd string, stdin io.Reader, stdout, stderr io.Writer) (int, string, error) {
	path := d.Command[0]
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	bin, err := readModule(path)
	if err != nil {
		return 0, "", err
	}

	memMiB := m.wasiMemoryMiB
	if memMiB <= 0 {
		memMiB = defaultWASIMemoryMiB
	}
	if memMiB > maxWASIMemoryMiB {
		memMiB = maxWASIMemoryMiB
	}
	cfg := wazero.NewRuntimeConfig().
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(uint32(memMiB << 20 / wasmPageBytes)).
		WithCompilationCache(m.wasiCache)
	rt := wazero.NewRuntimeWithConfig(ctx, cfg)
	defer func() { _ = rt.Close(context.Background()) }()
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		return stoppedOr(ctx, err)
	}
	mu, _ := m.wasiCompiling.LoadOrStore(sha256.Sum256(bin), &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	compiled, err := rt.CompileModule(ctx, bin)
	mu.(*sync.Mutex).Unlock()
	if err != nil {
		return stoppedOr(ctx, fmt.Errorf("compile %s: %w", d.Command[0], err))
	}

	mc := wazero.NewModuleConfig().
		WithName("").
		WithArgs(d.Command...).
		WithStdin(stdin).
		WithStdout(stdout).
		WithStderr(stderr).
		WithFSConfig(wazero.NewFSConfig().WithDirMount(cwd, "/")).
		WithSysWalltime().
		WithSysNanotime().
		WithSysNanosleep().
		WithRandSource(rand.Reader)
	for k, v := range d.Environment {
		mc = mc.WithEnv(k, v)
	}

	m.mu.Lock()
	if p.state == "STARTING" {
		p.state = "RUNNING"
	}
	m.mu.Unlock()

	_, err = rt.InstantiateModule(ctx, compiled, mc)
	if sc, ok := context.Cause(ctx).(signalCause); ok {
		return 128 + int(sc.sig), signalName(sc.sig), nil
	}
	var exit *sys.ExitError
	switch {
	case err == nil:
		return 0, "", nil
	case errors.As(err, &exit):
		return int(exit.ExitCode()), "", nil
	default:
		// A trap (unreachable, out-of-bounds access, stack exhaustion, memory
		// limit) is the module crashing: exit 1 with the reason on stderr.
		_, _ = fmt.Fprintf(stderr, "ignition: wasi: %v\n", err)
		return 1, "", nil
	}
}

// stoppedOr reports a pre-start failure, unless the failure is the module
// being signaled or canceled while it compiled: that ends it EXITED with the
// signal, the same as stopping a module that was already running.
func stoppedOr(ctx context.Context, err error) (int, string, error) {
	if sc, ok := context.Cause(ctx).(signalCause); ok {
		return 128 + int(sc.sig), signalName(sc.sig), nil
	}
	return 0, "", err
}

func readModule(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	bin, err := io.ReadAll(io.LimitReader(f, maxWASIModuleBytes+1))
	if err != nil {
		return nil, err
	}
	if len(bin) > maxWASIModuleBytes {
		return nil, fmt.Errorf("%s: module exceeds %d bytes", path, maxWASIModuleBytes)
	}
	return bin, nil
}
