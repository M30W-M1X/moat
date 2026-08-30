package vm

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/sirupsen/logrus"

	"github.com/M30W-M1X/moat/internal/netconf"
)

// Reconcile finds Firecracker processes under runRoot that no owner claims and
// removes them. For each run/<id> directory whose id is not in keep, it kills
// the recorded Firecracker pid, tears down the network, and removes the
// directory. It returns the ids it reaped.
//
// Run it at controller startup: a crash leaves running VMMs and their run
// directories behind, and the lease registry (keep) is the source of truth.
func Reconcile(runRoot string, keep map[string]bool, log *logrus.Logger) []string {
	entries, err := os.ReadDir(runRoot)
	if err != nil {
		return nil
	}
	var reaped []string
	for _, e := range entries {
		if !e.IsDir() || keep[e.Name()] {
			continue
		}
		id := e.Name()
		dir := filepath.Join(runRoot, id)

		if pid := readPID(filepath.Join(dir, "firecracker.pid")); pid > 0 && isFirecracker(pid) {
			log.WithFields(logrus.Fields{"id": id, "pid": pid}).Warn("reaping orphan microVM")
			_ = syscall.Kill(pid, syscall.SIGKILL)
			reaped = append(reaped, id)
		}
		netconf.DestroyByID(id)
		_ = os.RemoveAll(dir)
	}
	return reaped
}

func readPID(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

// isFirecracker reports whether pid is alive and is a firecracker process. It
// avoids killing a pid that the OS has recycled for something else.
func isFirecracker(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	return strings.Contains(strings.ReplaceAll(string(b), "\x00", " "), "firecracker")
}
