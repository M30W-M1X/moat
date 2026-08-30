//go:build integration

package vm_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/M30W-M1X/moat/internal/cgroup"
	"github.com/M30W-M1X/moat/internal/config"
	"github.com/M30W-M1X/moat/internal/vm"
)

// TestBootWithCgroupLimits boots a microVM with a CPU cap and a disk bandwidth
// limit, then checks that the cgroup file holds the expected value and that Stop
// removes the cgroup.
func TestBootWithCgroupLimits(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("no /dev/kvm")
	}
	if err := cgroup.Available(); err != nil {
		t.Skipf("cgroup delegation not available: %v", err)
	}
	m, err := config.NewMachine("it-limits")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Skipf("images missing (run: make images): %v", err)
	}
	_ = os.RemoveAll(m.RunDir)
	m.Limits = cgroup.Limits{CPUCores: 0.5, MemBytes: 200 << 20, PIDs: 128}
	m.DiskBWBytesPerSec = 8 << 20

	log := quietLog()
	inst, err := vm.Boot(context.Background(), m, log)
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}

	dir := cgroupDirFor("mvm-it-limits")
	if b, err := os.ReadFile(filepath.Join(dir, "cpu.max")); err != nil {
		t.Fatalf("read cpu.max at %s: %v", dir, err)
	} else if strings.TrimSpace(string(b)) != "50000 100000" {
		t.Errorf("cpu.max = %q, want %q", strings.TrimSpace(string(b)), "50000 100000")
	}
	if b, err := os.ReadFile(filepath.Join(dir, "cgroup.procs")); err != nil || len(strings.TrimSpace(string(b))) == 0 {
		t.Errorf("cgroup.procs empty or unreadable: %v", err)
	}

	sc, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	_ = inst.Stop(sc)
	cancel()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("cgroup %s still present after Stop", dir)
}

// cgroupDirFor mirrors cgroup.Apply's path layout for the assertion.
func cgroupDirFor(name string) string {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "0::") {
			continue
		}
		parts := strings.Split(strings.Trim(strings.TrimPrefix(line, "0::"), "/"), "/")
		for i, p := range parts {
			if strings.HasPrefix(p, "user@") && strings.HasSuffix(p, ".service") {
				return filepath.Join(append([]string{"/sys/fs/cgroup"}, append(parts[:i+1], "microvm", name)...)...)
			}
		}
	}
	return filepath.Join("/sys/fs/cgroup/microvm", name)
}
