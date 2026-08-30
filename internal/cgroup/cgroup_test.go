package cgroup

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLimitsZero(t *testing.T) {
	if !(Limits{}).Zero() {
		t.Error("empty Limits should be Zero")
	}
	for _, l := range []Limits{{CPUCores: 0.5}, {MemBytes: 1}, {PIDs: 1}} {
		if l.Zero() {
			t.Errorf("%+v should not be Zero", l)
		}
	}
}

func TestApplyAndRemove(t *testing.T) {
	if err := Available(); err != nil {
		t.Skipf("cgroup delegation not available: %v", err)
	}

	cmd := exec.Command("sleep", "120")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	remove, err := Apply("test-"+t.Name(), pid, Limits{CPUCores: 0.5, MemBytes: 64 << 20, PIDs: 50})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	root, _ := delegatedRoot()
	dir := filepath.Join(root, "microvm", "test-"+t.Name())

	checks := map[string]string{
		"cpu.max":    "50000 100000",
		"memory.max": "67108864",
		"pids.max":   "50",
	}
	for file, want := range checks {
		got, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if strings.TrimSpace(string(got)) != want {
			t.Errorf("%s = %q, want %q", file, strings.TrimSpace(string(got)), want)
		}
	}

	procs, _ := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	if !strings.Contains(string(procs), itoa(pid)) {
		t.Errorf("cgroup.procs %q does not contain pid %d", procs, pid)
	}

	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	remove()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("cgroup dir %s still present after remove()", dir)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
