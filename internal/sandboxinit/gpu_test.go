package sandboxinit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ignition.dev/ignition/internal/gpuid"
)

const canonicalUUID = "GPU-4a1b2c3d-1122-3344-5566-778899aabbcc"

// fakeDev builds a directory with the given device-node names.
func fakeDev(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func probeWith(dev string, smiOut string, smiErr error) gpuProbe {
	return gpuProbe{
		devDir:    dev,
		nvidiaSMI: "nvidia-smi",
		cudaCheck: "", // no helper in unit tests
		timeout:   time.Second,
		execCmd: func(_ context.Context, name string, _ ...string) ([]byte, error) {
			if strings.Contains(name, "nvidia-smi") {
				return []byte(smiOut), smiErr
			}
			return nil, nil
		},
	}
}

func TestGPUProbeHappyPath(t *testing.T) {
	dev := fakeDev(t, "nvidiactl", "nvidia-uvm", "nvidia0")
	p := probeWith(dev, canonicalUUID+", 0\n", nil)
	got, err := p.run(context.Background())
	if err != nil || got != canonicalUUID {
		t.Fatalf("got %q err=%v", got, err)
	}
}

func TestGPUProbeRejectsNonCanonicalUUID(t *testing.T) {
	dev := fakeDev(t, "nvidiactl", "nvidia-uvm", "nvidia0")
	p := probeWith(dev, "GPU-1, 0\n", nil)
	if _, err := p.run(context.Background()); err == nil {
		t.Fatal("accepted non-canonical UUID")
	}
}

func TestGPUProbeRejectsMIGUUID(t *testing.T) {
	dev := fakeDev(t, "nvidiactl", "nvidia-uvm", "nvidia0")
	p := probeWith(dev, "MIG-4a1b2c3d-1122-3344-5566-778899aabbcc, 0\n", nil)
	if _, err := p.run(context.Background()); err == nil {
		t.Fatal("accepted MIG UUID")
	}
}

func TestGPUProbeRejectsUncorrectedECC(t *testing.T) {
	dev := fakeDev(t, "nvidiactl", "nvidia-uvm", "nvidia0")
	p := probeWith(dev, canonicalUUID+", 3\n", nil)
	if _, err := p.run(context.Background()); err == nil {
		t.Fatal("accepted GPU with uncorrected ECC errors")
	}
}

func TestGPUProbeAllowsUnknownECC(t *testing.T) {
	dev := fakeDev(t, "nvidiactl", "nvidia-uvm", "nvidia0")
	p := probeWith(dev, canonicalUUID+", [N/A]\n", nil)
	if _, err := p.run(context.Background()); err != nil {
		t.Fatalf("unknown ECC counter should not fail: %v", err)
	}
}

func TestGPUProbeRejectsMultipleGPURows(t *testing.T) {
	dev := fakeDev(t, "nvidiactl", "nvidia-uvm", "nvidia0")
	p := probeWith(dev, canonicalUUID+", 0\nGPU-99999999-0000-0000-0000-000000000000, 0\n", nil)
	if _, err := p.run(context.Background()); err == nil {
		t.Fatal("accepted more than one GPU")
	}
}

func TestGPUProbeRejectsSMIError(t *testing.T) {
	dev := fakeDev(t, "nvidiactl", "nvidia-uvm", "nvidia0")
	p := probeWith(dev, "", errors.New("exit status 9"))
	if _, err := p.run(context.Background()); err == nil {
		t.Fatal("accepted nvidia-smi failure")
	}
}

func TestGPUProbeRequiresDeviceNodes(t *testing.T) {
	// nvidia-uvm missing -> fails before nvidia-smi is consulted.
	dev := fakeDev(t, "nvidiactl", "nvidia0")
	p := probeWith(dev, canonicalUUID+", 0\n", nil)
	if _, err := p.run(context.Background()); err == nil {
		t.Fatal("accepted a sandbox with no /dev/nvidia-uvm")
	}
}

func TestGPUProbeRequiresExactlyOneDeviceNode(t *testing.T) {
	dev := fakeDev(t, "nvidiactl", "nvidia-uvm", "nvidia0", "nvidia1")
	p := probeWith(dev, canonicalUUID+", 0\n", nil)
	if _, err := p.run(context.Background()); err == nil {
		t.Fatal("accepted two GPU device nodes")
	}
}

func TestGPUProbeCUDACheckFailure(t *testing.T) {
	dev := fakeDev(t, "nvidiactl", "nvidia-uvm", "nvidia0")
	helper := filepath.Join(t.TempDir(), "cuda-check")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := probeWith(dev, canonicalUUID+", 0\n", nil)
	p.cudaCheck = helper
	p.execCmd = func(_ context.Context, name string, _ ...string) ([]byte, error) {
		if strings.Contains(name, "nvidia-smi") {
			return []byte(canonicalUUID + ", 0\n"), nil
		}
		return nil, errors.New("cuInit failed")
	}
	if _, err := p.run(context.Background()); err == nil {
		t.Fatal("readiness passed despite a failing CUDA check")
	}
}

func TestReadyzSurfacesCanonicalUUID(t *testing.T) {
	if !gpuid.IsCanonical(canonicalUUID) {
		t.Fatal("test UUID is not canonical")
	}
}
