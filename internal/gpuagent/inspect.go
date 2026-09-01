package gpuagent

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// GPU is one physical accelerator as reported by nvidia-smi.
type GPU struct {
	UUID           string
	PCIBusID       string
	ECCUncorrected int  // count of volatile uncorrected ECC errors; -1 when the card does not expose the counter
	ResetRequired  bool // the driver flagged the GPU as needing a reset
}

// ComputeProc is one running CUDA process on the node's GPU.
type ComputeProc struct {
	PID     int
	UsedMiB int
}

// HealthOK reports whether a GPU is fit to lease. An unknown ECC counter (-1)
// is not a failure; a positive count or a pending reset is.
func HealthOK(g GPU) bool {
	return g.ECCUncorrected <= 0 && !g.ResetRequired
}

// Inspector is the node-local GPU truth source. The production implementation
// shells nvidia-smi; tests supply a fake.
type Inspector interface {
	Inventory(ctx context.Context) ([]GPU, error)
	ComputeProcesses(ctx context.Context) ([]ComputeProc, error)
}

// smiInspector runs nvidia-smi from the driver install mounted into the agent
// Pod. It never reads environment variables or device-node names.
type smiInspector struct {
	bin     string
	timeout time.Duration
	run     func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// NewSMIInspector returns an Inspector backed by the nvidia-smi at bin
// (default "nvidia-smi" when empty).
func NewSMIInspector(bin string) Inspector {
	if bin == "" {
		bin = "nvidia-smi"
	}
	return &smiInspector{bin: bin, timeout: 10 * time.Second, run: runCommand}
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

func (s *smiInspector) Inventory(ctx context.Context) ([]GPU, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	out, err := s.run(ctx, s.bin,
		"--query-gpu=uuid,pci.bus_id,ecc.errors.uncorrected.volatile.total,gpu_reset_status.reset_required",
		"--format=csv,noheader,nounits")
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi --query-gpu: %w", err)
	}
	var gpus []GPU
	for _, line := range nonEmptyLines(string(out)) {
		f := splitCSV(line)
		if len(f) == 0 || f[0] == "" {
			continue
		}
		g := GPU{UUID: f[0], ECCUncorrected: -1}
		if len(f) > 1 {
			g.PCIBusID = f[1]
		}
		if len(f) > 2 {
			if n, perr := strconv.Atoi(f[2]); perr == nil {
				g.ECCUncorrected = n
			}
		}
		if len(f) > 3 {
			g.ResetRequired = strings.EqualFold(f[3], "yes") || strings.EqualFold(f[3], "true")
		}
		gpus = append(gpus, g)
	}
	return gpus, nil
}

func (s *smiInspector) ComputeProcesses(ctx context.Context) ([]ComputeProc, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	out, err := s.run(ctx, s.bin,
		"--query-compute-apps=pid,used_gpu_memory",
		"--format=csv,noheader,nounits")
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi --query-compute-apps: %w", err)
	}
	var procs []ComputeProc
	for _, line := range nonEmptyLines(string(out)) {
		// Some driver versions print a human sentence when idle.
		if strings.Contains(strings.ToLower(line), "no running processes") {
			continue
		}
		f := splitCSV(line)
		pid, perr := strconv.Atoi(f[0])
		if perr != nil {
			continue
		}
		p := ComputeProc{PID: pid}
		if len(f) > 1 {
			p.UsedMiB, _ = strconv.Atoi(f[1])
		}
		procs = append(procs, p)
	}
	return procs, nil
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
