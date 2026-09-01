package gpuagent

import (
	"context"
	"strings"
	"testing"
	"time"
)

func smiWith(t *testing.T, byArgs map[string]string) *smiInspector {
	t.Helper()
	return &smiInspector{
		bin:     "nvidia-smi",
		timeout: time.Second,
		run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			joined := strings.Join(args, " ")
			for key, out := range byArgs {
				if strings.Contains(joined, key) {
					return []byte(out), nil
				}
			}
			return nil, nil
		},
	}
}

func TestInventoryParsesHealthFields(t *testing.T) {
	s := smiWith(t, map[string]string{
		"query-gpu": "GPU-4a1b2c3d-1122-3344-5566-778899aabbcc, 00000000:00:04.0, 0, No\n",
	})
	gpus, err := s.Inventory(context.Background())
	if err != nil || len(gpus) != 1 {
		t.Fatalf("gpus=%v err=%v", gpus, err)
	}
	g := gpus[0]
	if g.ECCUncorrected != 0 || g.ResetRequired || g.PCIBusID != "00000000:00:04.0" {
		t.Fatalf("bad parse: %+v", g)
	}
	if !HealthOK(g) {
		t.Fatal("healthy GPU reported unhealthy")
	}
}

func TestInventoryFlagsResetRequired(t *testing.T) {
	s := smiWith(t, map[string]string{
		"query-gpu": "GPU-4a1b2c3d-1122-3344-5566-778899aabbcc, 00000000:00:04.0, [N/A], Yes\n",
	})
	gpus, _ := s.Inventory(context.Background())
	if len(gpus) != 1 || !gpus[0].ResetRequired || HealthOK(gpus[0]) {
		t.Fatalf("reset-required not honored: %+v", gpus)
	}
	if gpus[0].ECCUncorrected != -1 {
		t.Fatalf("unknown ECC should be -1, got %d", gpus[0].ECCUncorrected)
	}
}

func TestComputeProcessesIdleSentence(t *testing.T) {
	s := smiWith(t, map[string]string{
		"query-compute-apps": "No running processes found\n",
	})
	procs, err := s.ComputeProcesses(context.Background())
	if err != nil || len(procs) != 0 {
		t.Fatalf("procs=%v err=%v", procs, err)
	}
}

func TestComputeProcessesParsesRows(t *testing.T) {
	s := smiWith(t, map[string]string{
		"query-compute-apps": "4242, 512\n5555, 1024\n",
	})
	procs, _ := s.ComputeProcesses(context.Background())
	if len(procs) != 2 || procs[0].PID != 4242 || procs[1].UsedMiB != 1024 {
		t.Fatalf("bad parse: %+v", procs)
	}
}
