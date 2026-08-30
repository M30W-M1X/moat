//go:build integration

package vm_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/M30W-M1X/microvm_controller/internal/agentclient"
	"github.com/M30W-M1X/microvm_controller/internal/config"
	"github.com/M30W-M1X/microvm_controller/internal/vm"
)

func quietLog() *logrus.Logger {
	l := logrus.New()
	l.SetLevel(logrus.WarnLevel)
	return l
}

func TestSnapshotRoundTrip(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("no /dev/kvm")
	}
	m, err := config.NewMachine("it-snap")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Skipf("images missing (run: make images): %v", err)
	}
	_ = os.RemoveAll(m.RunDir)
	log := quietLog()
	ctx := context.Background()

	inst, err := vm.Boot(ctx, m, log)
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	cli, err := agentclient.Dial(m.VsockUDSPath(), config.VsockPort)
	if err != nil {
		t.Fatal(err)
	}
	rc, cancel := context.WithTimeout(ctx, 20*time.Second)
	if err := cli.WaitReady(rc); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	cancel()

	// Leave a marker on disk, then snapshot.
	if _, err := cli.Exec(ctx, agentclient.ExecOptions{
		Argv: []string{"sh", "-c", "echo marker-123 > /root/m && sync"},
	}, nil, nil); err != nil {
		t.Fatalf("prep exec: %v", err)
	}
	snapDir := filepath.Join(m.RunDir, "snap")
	if err := inst.CreateSnapshot(ctx, snapDir); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	cli.Close()
	sc, c := context.WithTimeout(ctx, 15*time.Second)
	_ = inst.Stop(sc)
	c()

	// Restore and check the marker survived.
	t0 := time.Now()
	r, err := vm.Restore(ctx, m, snapDir, log)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	t.Cleanup(func() {
		sc, c := context.WithTimeout(context.Background(), 15*time.Second)
		_ = r.Stop(sc)
		c()
	})
	rcli, err := agentclient.Dial(m.VsockUDSPath(), config.VsockPort)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rcli.Close() })
	rc2, cancel2 := context.WithTimeout(ctx, 15*time.Second)
	if err := rcli.WaitReady(rc2); err != nil {
		t.Fatalf("agent not ready after restore: %v", err)
	}
	cancel2()
	t.Logf("restore -> agent ready in %s", time.Since(t0).Round(time.Millisecond))

	var out []byte
	res, err := rcli.Exec(ctx, agentclient.ExecOptions{Argv: []string{"cat", "/root/m"}}, writerFunc(func(b []byte) { out = append(out, b...) }), nil)
	if err != nil || res.Code != 0 {
		t.Fatalf("read marker: err=%v code=%d", err, res.Code)
	}
	if string(out) != "marker-123\n" {
		t.Errorf("marker after restore = %q, want %q", out, "marker-123\n")
	}
}

func TestReconcileKillsOrphan(t *testing.T) {
	root := t.TempDir()
	// A long-lived helper process stands in for an orphaned Firecracker. It is
	// not actually firecracker, so Reconcile should NOT kill it, but it should
	// still remove the directory.
	cmd := exec.Command("sleep", "300")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()

	id := "stale-1"
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "firecracker.pid"), []byte("999999999"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A dir that IS in the keep set must be left alone.
	keepDir := filepath.Join(root, "keep-me")
	_ = os.MkdirAll(keepDir, 0o755)

	vm.Reconcile(root, map[string]bool{"keep-me": true}, quietLog())

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("orphan dir not removed: %v", err)
	}
	if _, err := os.Stat(keepDir); err != nil {
		t.Errorf("kept dir was removed: %v", err)
	}
}

type writerFunc func([]byte)

func (w writerFunc) Write(b []byte) (int, error) { w(b); return len(b), nil }
