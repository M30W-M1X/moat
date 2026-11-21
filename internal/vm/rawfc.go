package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/sirupsen/logrus"

	"github.com/M30W-M1X/moat/internal/config"
)

// UFFDAvailable reports whether Firecracker can create a userfaultfd object on
// this host. It needs read access to /dev/userfaultfd, or
// vm.unprivileged_userfaultfd = 1.
func UFFDAvailable() error {
	if b, err := os.ReadFile("/proc/sys/vm/unprivileged_userfaultfd"); err == nil {
		if len(b) > 0 && b[0] == '1' {
			return nil
		}
	}
	f, err := os.Open("/dev/userfaultfd")
	if err != nil {
		return fmt.Errorf("userfaultfd not available (run as root, or set "+
			"vm.unprivileged_userfaultfd=1): %w", err)
	}
	f.Close()
	return nil
}

// RestoreUFFD starts a Firecracker microVM from a snapshot with the userfaultfd
// memory backend. Guest RAM is not read up front: a page-fault handler
// (moat-uffd) streams pages from mem.bin on demand. This keeps restore
// latency and host memory flat regardless of guest size.
//
// uffdBin is the path to the moat-uffd binary; empty resolves it on PATH.
// UFFDAvailable must pass first.
func RestoreUFFD(ctx context.Context, m config.Machine, snapDir, uffdBin string, log *logrus.Logger) (*Instance, error) {
	if err := UFFDAvailable(); err != nil {
		return nil, err
	}
	snapPath := filepath.Join(snapDir, "snapshot.bin")
	memPath := filepath.Join(snapDir, "mem.bin")
	for _, p := range []string{snapPath, memPath} {
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("snapshot file %q: %w", p, err)
		}
	}
	fcBin, err := resolveBin(m.FirecrackerBin, "firecracker")
	if err != nil {
		return nil, err
	}
	if uffdBin == "" {
		if uffdBin, err = exec.LookPath("moat-uffd"); err != nil {
			return nil, fmt.Errorf("moat-uffd not found on PATH: %w", err)
		}
	}
	if err := os.MkdirAll(m.RunDir, 0o755); err != nil {
		return nil, err
	}
	vsockPath := resolveSnapshotVsockPath(snapDir, snapPath, m.VsockUDSPath())
	if err := prepareVsockPath(vsockPath); err != nil {
		return nil, err
	}
	apiSock := m.APISocketPath()
	uffdSock := filepath.Join(m.RunDir, "uffd.sock")
	for _, s := range []string{apiSock, uffdSock, m.VsockUDSPath()} {
		_ = os.Remove(s)
	}

	in := &Instance{Machine: m, log: log, vsockPath: vsockPath}

	// 1. Start the page-fault handler and wait for its socket.
	in.rawUFFD = exec.CommandContext(ctx, uffdBin, "--socket", uffdSock, "--mem", memPath)
	in.rawUFFD.Stdout, in.rawUFFD.Stderr = isolateStdio(os.Stdout, os.Stderr)
	if err := in.rawUFFD.Start(); err != nil {
		return nil, fmt.Errorf("start moat-uffd: %w", err)
	}
	if err := waitForFile(uffdSock, 3*time.Second); err != nil {
		_ = in.stopRaw()
		return nil, fmt.Errorf("moat-uffd socket: %w", err)
	}

	// 2. Start Firecracker, inside the network namespace when one is set.
	start := func() error {
		in.rawFC = exec.Command(fcBin, "--api-sock", apiSock)
		in.rawFC.Stdout, in.rawFC.Stderr = isolateStdio(os.Stdout, os.Stderr)
		return in.rawFC.Start()
	}
	if m.Net != nil {
		err = ns.WithNetNSPath(m.Net.NetnsPath, func(ns.NetNS) error { return start() })
	} else {
		err = start()
	}
	if err != nil {
		_ = in.stopRaw()
		return nil, fmt.Errorf("start firecracker: %w", err)
	}
	in.pid = in.rawFC.Process.Pid
	_ = os.WriteFile(m.PIDPath(), []byte(strconv.Itoa(in.pid)), 0o644)

	api := fcAPIClient(apiSock)
	if err := waitForAPI(ctx, api, 5*time.Second); err != nil {
		_ = in.stopRaw()
		return nil, fmt.Errorf("firecracker API: %w", err)
	}

	// 3. Load the snapshot with the UFFD backend and resume.
	body := map[string]any{
		"snapshot_path":         snapPath,
		"mem_backend":           map[string]string{"backend_path": uffdSock, "backend_type": "Uffd"},
		"enable_diff_snapshots": false,
		"resume_vm":             true,
	}
	if err := api.put(ctx, "/snapshot/load", body); err != nil {
		_ = in.stopRaw()
		return nil, fmt.Errorf("load snapshot: %w", err)
	}
	return in, nil
}

// stopRaw kills the Firecracker and handler processes started by RestoreUFFD.
func (in *Instance) stopRaw() error {
	for _, c := range []*exec.Cmd{in.rawFC, in.rawUFFD} {
		if c != nil && c.Process != nil {
			_ = c.Process.Kill()
			_ = c.Wait()
		}
	}
	_ = os.Remove(in.Machine.APISocketPath())
	_ = os.Remove(in.VsockUDSPath())
	_ = os.Remove(filepath.Join(in.Machine.RunDir, "uffd.sock"))
	_ = os.Remove(in.Machine.PIDPath())
	in.rawFC, in.rawUFFD = nil, nil
	if in.cgroupRemove != nil {
		in.cgroupRemove()
		in.cgroupRemove = nil
	}
	return nil
}

// --- minimal Firecracker API client over the unix socket ---

type fcAPI struct {
	c    *http.Client
	base string
}

func fcAPIClient(sock string) *fcAPI {
	return &fcAPI{
		base: "http://localhost",
		c: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sock)
			},
		}},
	}
}

func (a *fcAPI) put(ctx context.Context, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, a.base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg := make([]byte, 512)
		n, _ := resp.Body.Read(msg)
		return fmt.Errorf("%s %s: %s: %s", http.MethodPut, path, resp.Status, msg[:n])
	}
	return nil
}

func waitForAPI(ctx context.Context, a *fcAPI, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, a.base+"/version", nil)
		if resp, err := a.c.Do(req); err == nil {
			resp.Body.Close()
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("not responding after %s", timeout)
}

func waitForFile(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("%s did not appear in %s", path, timeout)
}
