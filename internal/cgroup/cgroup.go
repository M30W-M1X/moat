// Package cgroup applies cgroup v2 CPU, memory, and PID limits to a running
// process (the Firecracker VMM). It writes into the caller's delegated cgroup
// subtree, so it works without root when systemd delegation is in effect, and
// returns a clear error otherwise.
package cgroup

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// mountBase is the cgroup v2 filesystem mount point.
const mountBase = "/sys/fs/cgroup"

// period is the cpu.max accounting window, in microseconds.
const period = 100_000

// Limits are the per-VM caps. A zero field means "no limit for this resource".
type Limits struct {
	CPUCores float64 // e.g. 0.5 caps the VM at half a core
	MemBytes int64
	PIDs     int64
}

// Zero reports whether every field is unset.
func (l Limits) Zero() bool {
	return l.CPUCores <= 0 && l.MemBytes <= 0 && l.PIDs <= 0
}

// Available returns nil when this process can create a limited cgroup.
func Available() error {
	root, err := delegatedRoot()
	if err != nil {
		return err
	}
	probe := filepath.Join(root, ".mvm-probe")
	if err := os.Mkdir(probe, 0o755); err != nil {
		return fmt.Errorf("cannot create a cgroup under %s (need systemd delegation or root): %w", root, err)
	}
	_ = os.Remove(probe)
	return nil
}

// Apply creates a cgroup named name, sets l on it, and moves pid into it. The
// returned function removes the cgroup; call it after the process has exited.
func Apply(name string, pid int, l Limits) (remove func(), err error) {
	if l.Zero() {
		return func() {}, nil
	}
	root, err := delegatedRoot()
	if err != nil {
		return nil, err
	}
	parent := filepath.Join(root, "microvm")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", parent, err)
	}
	// Best effort: hand the needed controllers down to the children.
	_ = writeFile(filepath.Join(root, "cgroup.subtree_control"), "+cpu +memory +pids")
	_ = writeFile(filepath.Join(parent, "cgroup.subtree_control"), "+cpu +memory +pids")

	dir := filepath.Join(parent, name)
	if err := os.Mkdir(dir, 0o755); err != nil && !os.IsExist(err) {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	cleanup := func() { removeDir(dir) }

	if l.CPUCores > 0 {
		quota := int64(l.CPUCores * period)
		if err := writeFile(filepath.Join(dir, "cpu.max"), fmt.Sprintf("%d %d", quota, period)); err != nil {
			cleanup()
			return nil, err
		}
	}
	if l.MemBytes > 0 {
		if err := writeFile(filepath.Join(dir, "memory.max"), strconv.FormatInt(l.MemBytes, 10)); err != nil {
			cleanup()
			return nil, err
		}
	}
	if l.PIDs > 0 {
		if err := writeFile(filepath.Join(dir, "pids.max"), strconv.FormatInt(l.PIDs, 10)); err != nil {
			cleanup()
			return nil, err
		}
	}
	if err := writeFile(filepath.Join(dir, "cgroup.procs"), strconv.Itoa(pid)); err != nil {
		cleanup()
		return nil, fmt.Errorf("move pid %d into %s: %w", pid, dir, err)
	}
	return cleanup, nil
}

// delegatedRoot returns the cgroup directory this process may create children
// under: its own systemd user-manager scope root (…/user@<uid>.service), or the
// cgroup root when running as the cgroup namespace root.
func delegatedRoot() (string, error) {
	f, err := os.Open("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	defer f.Close()
	var rel string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// cgroup v2 line: "0::/user.slice/…/app.slice/foo.scope"
		if strings.HasPrefix(sc.Text(), "0::") {
			rel = strings.TrimPrefix(sc.Text(), "0::")
			break
		}
	}
	if rel == "" {
		return "", fmt.Errorf("no cgroup v2 membership in /proc/self/cgroup")
	}
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	for i, p := range parts {
		if strings.HasPrefix(p, "user@") && strings.HasSuffix(p, ".service") {
			return filepath.Join(append([]string{mountBase}, parts[:i+1]...)...), nil
		}
	}
	// No user manager: fall back to the cgroup root (needs privilege).
	return mountBase, nil
}

func writeFile(path, val string) error {
	if err := os.WriteFile(path, []byte(val), 0o644); err != nil {
		return fmt.Errorf("write %s = %q: %w", filepath.Base(path), val, err)
	}
	return nil
}

// removeDir retries rmdir: the kernel refuses it until the last process leaves.
func removeDir(dir string) {
	for i := 0; i < 20; i++ {
		if err := os.Remove(dir); err == nil || os.IsNotExist(err) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
