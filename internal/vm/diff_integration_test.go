//go:build integration

package vm_test

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/M30W-M1X/moat/internal/agentclient"
	"github.com/M30W-M1X/moat/internal/config"
	"github.com/M30W-M1X/moat/internal/vm"
)

// TestDiffSnapshotRoundTrip takes a full snapshot, restores it with dirty-page
// tracking, changes the guest, takes a diff snapshot, and restores from the
// base plus the diff. Both markers must survive, and the diff memory file must
// occupy far less disk than the full one.
func TestDiffSnapshotRoundTrip(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("no /dev/kvm")
	}
	m, err := config.NewMachine("it-diff")
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

	if _, err := cli.Exec(ctx, agentclient.ExecOptions{
		Argv: []string{"sh", "-c", "echo base-marker > /root/base && sync"},
	}, nil, nil); err != nil {
		t.Fatalf("base exec: %v", err)
	}
	baseDir := filepath.Join(m.RunDir, "snap")
	if err := inst.CreateSnapshot(ctx, baseDir); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	cli.Close()
	sc, c := context.WithTimeout(ctx, 15*time.Second)
	_ = inst.Stop(sc)
	c()

	// Restore the full snapshot in a fresh process with dirty tracking, change
	// the guest, then write a diff snapshot.
	d, err := vm.RestoreForDiff(ctx, m, baseDir, log)
	if err != nil {
		t.Fatalf("RestoreForDiff: %v", err)
	}
	dcli, err := agentclient.Dial(m.VsockUDSPath(), config.VsockPort)
	if err != nil {
		t.Fatal(err)
	}
	drc, dcancel := context.WithTimeout(ctx, 15*time.Second)
	if err := dcli.WaitReady(drc); err != nil {
		t.Fatalf("agent not ready after RestoreForDiff: %v", err)
	}
	dcancel()
	if _, err := dcli.Exec(ctx, agentclient.ExecOptions{
		Argv: []string{"sh", "-c", "echo diff-marker > /root/diff && sync"},
	}, nil, nil); err != nil {
		t.Fatalf("diff exec: %v", err)
	}
	diffDir := filepath.Join(baseDir, "diff")
	if err := d.CreateSnapshotDiff(ctx, diffDir); err != nil {
		t.Fatalf("CreateSnapshotDiff: %v", err)
	}
	dcli.Close()
	sc, c = context.WithTimeout(ctx, 15*time.Second)
	_ = d.Stop(sc)
	c()

	baseMem := diskBytes(t, filepath.Join(baseDir, "mem.bin"))
	diffMem := diskBytes(t, filepath.Join(diffDir, "mem.bin"))
	t.Logf("base mem on disk %d bytes, diff mem on disk %d bytes", baseMem, diffMem)
	if diffMem >= baseMem {
		t.Errorf("diff memory (%d) not smaller than base memory (%d) on disk", diffMem, baseMem)
	}

	// Restore from base + diff and read both markers.
	t0 := time.Now()
	r, err := vm.RestoreDiff(ctx, m, baseDir, diffDir, log)
	if err != nil {
		t.Fatalf("RestoreDiff: %v", err)
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
		t.Fatalf("agent not ready after diff restore: %v", err)
	}
	cancel2()
	t.Logf("diff restore -> agent ready in %s", time.Since(t0).Round(time.Millisecond))

	var out []byte
	res, err := rcli.Exec(ctx, agentclient.ExecOptions{Argv: []string{"cat", "/root/base", "/root/diff"}},
		writerFunc(func(b []byte) { out = append(out, b...) }), nil)
	if err != nil || res.Code != 0 {
		t.Fatalf("read markers: err=%v code=%d", err, res.Code)
	}
	if string(out) != "base-marker\ndiff-marker\n" {
		t.Errorf("markers after diff restore = %q, want %q", out, "base-marker\ndiff-marker\n")
	}
}

// TestRebaseMemory checks the sparse-overlay merge on plain files, with no VM.
func TestRebaseMemory(t *testing.T) {
	dir := t.TempDir()
	const size = 1 << 20 // 1 MiB

	basePath := filepath.Join(dir, "base")
	base := make([]byte, size)
	for i := range base {
		base[i] = 'A'
	}
	if err := os.WriteFile(basePath, base, 0o644); err != nil {
		t.Fatal(err)
	}

	// A sparse diff: one populated 4 KiB page at offset 8 KiB, rest holes.
	diffPath := filepath.Join(dir, "diff")
	df, err := os.Create(diffPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := df.Truncate(size); err != nil {
		t.Fatal(err)
	}
	page := make([]byte, 4096)
	for i := range page {
		page[i] = 'B'
	}
	if _, err := df.WriteAt(page, 8192); err != nil {
		t.Fatal(err)
	}
	df.Close()

	outPath := filepath.Join(dir, "out")
	if err := vm.RebaseMemory(basePath, diffPath, outPath); err != nil {
		t.Fatalf("RebaseMemory: %v", err)
	}

	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != size {
		t.Fatalf("merged size = %d, want %d", len(got), size)
	}
	for i := 0; i < size; i++ {
		want := byte('A')
		if i >= 8192 && i < 8192+4096 {
			want = 'B'
		}
		if got[i] != want {
			t.Fatalf("byte %d = %q, want %q", i, got[i], want)
		}
	}
}

func diskBytes(t *testing.T, path string) int64 {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return st.Blocks * 512
}
