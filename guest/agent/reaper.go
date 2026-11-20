package main

import (
	"bytes"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// childTracker tracks direct child processes owned by Exec so the PID 1 zombie
// reaper never steals an exit status from exec.Cmd.Wait.
type childTracker struct {
	mu     sync.Mutex
	active map[int]struct{}
}

var children = &childTracker{active: map[int]struct{}{}}

// startAndTrack runs start while holding the tracker lock and records the
// returned pid before releasing it.
func (c *childTracker) startAndTrack(start func() (int, error)) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	pid, err := start()
	if err != nil {
		return 0, err
	}
	c.active[pid] = struct{}{}
	return pid, nil
}

// untrack removes pid after exec.Cmd.Wait has reaped it.
func (c *childTracker) untrack(pid int) {
	if pid <= 0 {
		return
	}
	c.mu.Lock()
	delete(c.active, pid)
	c.mu.Unlock()
}

// startZombieReaper listens for SIGCHLD and reaps orphaned zombie processes
// reparented to PID 1.
func startZombieReaper() {
	ch := make(chan os.Signal, 32)
	signal.Notify(ch, syscall.SIGCHLD)
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ch:
				children.reapOrphans("/proc", waitNoHang)
			case <-t.C:
				children.reapOrphans("/proc", waitNoHang)
			}
		}
	}()
}

func waitNoHang(pid int) {
	var ws syscall.WaitStatus
	_, _ = syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
}

// reapOrphans scans procDir for zombie processes whose parent is PID 1 and
// which are not tracked as active Exec children, then calls reap on each pid.
func (c *childTracker) reapOrphans(procDir string, reap func(int)) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entries, err := os.ReadDir(procDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 {
			continue
		}
		if _, tracked := c.active[pid]; tracked {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(procDir, e.Name(), "stat"))
		if err != nil {
			continue
		}
		state, ppid, ok := parseProcStat(raw)
		if ok && state == 'Z' && ppid == 1 {
			reap(pid)
		}
	}
}

// parseProcStat extracts the process state and parent pid from a Linux
// /proc/<pid>/stat line. The comm field may contain spaces and parentheses, so
// it locates the final ')' before reading the following fields.
func parseProcStat(raw []byte) (state byte, ppid int, ok bool) {
	idx := bytes.LastIndexByte(raw, ')')
	if idx < 0 || idx+2 >= len(raw) {
		return 0, 0, false
	}
	fields := bytes.Fields(raw[idx+1:])
	if len(fields) < 2 || len(fields[0]) == 0 {
		return 0, 0, false
	}
	p, err := strconv.Atoi(string(fields[1]))
	if err != nil {
		return 0, 0, false
	}
	return fields[0][0], p, true
}
