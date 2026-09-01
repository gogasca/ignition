package sandboxinit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"ignition.dev/ignition/internal/gpuid"
)

// gpuProbe verifies, without trusting environment variables or device-node
// names, that exactly one healthy GPU is mapped into this sandbox and that the
// CUDA stack initializes. The canonical UUID it returns is advisory: the
// authoritative identity and the residual-process verdict come from
// ignition-gpu-agent via Pod annotations the controller gates READY on.
type gpuProbe struct {
	devDir    string        // directory holding the nvidia device nodes ("/dev")
	nvidiaSMI string        // nvidia-smi binary name or path
	cudaCheck string        // cuda-check helper path; "" or absent skips the cuInit() check
	timeout   time.Duration // per-command deadline
	execCmd   func(ctx context.Context, name string, args ...string) ([]byte, error)
}

func defaultGPUProbe() gpuProbe {
	return gpuProbe{
		devDir:    "/dev",
		nvidiaSMI: envOr("IGNITION_NVIDIA_SMI", "nvidia-smi"),
		cudaCheck: envOr("IGNITION_CUDA_CHECK", "/ignition/cuda-check"),
		timeout:   10 * time.Second,
		execCmd:   runCommand,
	}
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

var nvidiaDeviceNode = regexp.MustCompile(`^nvidia[0-9]+$`)

// run executes the full probe and returns the GPU UUID on success. It fails
// closed: any missing device node, nvidia-smi error, non-canonical UUID,
// uncorrected ECC error, or CUDA-init failure yields an error and keeps
// /readyz at 503.
func (p gpuProbe) run(ctx context.Context) (string, error) {
	if err := p.checkDeviceNodes(); err != nil {
		return "", err
	}
	uuid, err := p.querySMI(ctx)
	if err != nil {
		return "", err
	}
	if err := p.checkCUDA(ctx); err != nil {
		return "", err
	}
	return uuid, nil
}

// checkDeviceNodes confirms the control interfaces and exactly one GPU device
// node are present. This establishes only that a GPU is plumbed into the
// namespace — never which GPU.
func (p gpuProbe) checkDeviceNodes() error {
	for _, n := range []string{"nvidiactl", "nvidia-uvm"} {
		if _, err := os.Stat(filepath.Join(p.devDir, n)); err != nil {
			return fmt.Errorf("missing /dev/%s: %w", n, err)
		}
	}
	entries, err := os.ReadDir(p.devDir)
	if err != nil {
		return fmt.Errorf("read %s: %w", p.devDir, err)
	}
	count := 0
	for _, e := range entries {
		if nvidiaDeviceNode.MatchString(e.Name()) {
			count++
		}
	}
	if count != 1 {
		return fmt.Errorf("expected exactly one GPU device node, found %d", count)
	}
	return nil
}

// querySMI runs nvidia-smi and returns the canonical GPU UUID, failing on any
// uncorrected ECC error or on anything other than exactly one GPU.
func (p gpuProbe) querySMI(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	out, err := p.execCmd(ctx, p.nvidiaSMI,
		"--query-gpu=uuid,ecc.errors.uncorrected.volatile.total",
		"--format=csv,noheader,nounits")
	if err != nil {
		return "", fmt.Errorf("nvidia-smi: %w", err)
	}
	rows := nonEmptyLines(string(out))
	if len(rows) != 1 {
		return "", fmt.Errorf("nvidia-smi reported %d GPUs, want exactly 1", len(rows))
	}
	fields := splitCSV(rows[0])
	if len(fields) == 0 || fields[0] == "" {
		return "", errors.New("nvidia-smi returned an empty row")
	}
	uuid := fields[0]
	if !gpuid.IsCanonical(uuid) {
		return "", fmt.Errorf("nvidia-smi returned a non-canonical GPU UUID %q", uuid)
	}
	if len(fields) >= 2 {
		// "[N/A]" or "" means the card does not expose volatile ECC counters;
		// only a parseable positive count is a failure.
		if n, perr := strconv.Atoi(fields[1]); perr == nil && n > 0 {
			return "", fmt.Errorf("GPU %s reports %d uncorrected ECC errors", uuid, n)
		}
	}
	return uuid, nil
}

// checkCUDA runs the cuda-check helper (cuInit + cuDeviceGetCount). It is a
// no-op when no helper is configured or present: the CPU sandbox image ships
// without it, and there nvidia-smi plus the device nodes are the signal.
func (p gpuProbe) checkCUDA(ctx context.Context) error {
	if p.cudaCheck == "" {
		return nil
	}
	if _, err := os.Stat(p.cudaCheck); err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	if _, err := p.execCmd(ctx, p.cudaCheck); err != nil {
		return fmt.Errorf("cuda-check: %w", err)
	}
	return nil
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func splitCSV(line string) []string {
	parts := strings.Split(line, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}
