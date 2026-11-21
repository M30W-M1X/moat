package vm

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestStdioIsolationHelper(t *testing.T) {
	if os.Getenv("MOAT_TEST_NONBLOCK_HELPER") != "1" {
		return
	}
	// Mirror Firecracker's DeviceManager::create_legacy_devices -> set_stdout_nonblocking.
	fl1, err := unix.FcntlInt(1, unix.F_GETFL, 0)
	if err != nil {
		t.Fatalf("fcntl(1, F_GETFL): %v", err)
	}
	if _, err := unix.FcntlInt(1, unix.F_SETFL, fl1|unix.O_NONBLOCK); err != nil {
		t.Fatalf("fcntl(1, F_SETFL, O_NONBLOCK): %v", err)
	}
	fl2, err := unix.FcntlInt(2, unix.F_GETFL, 0)
	if err != nil {
		t.Fatalf("fcntl(2, F_GETFL): %v", err)
	}
	if fl2&unix.O_NONBLOCK != 0 {
		t.Fatalf("O_NONBLOCK leaked from fd 1 onto fd 2 (flags=0x%x)", fl2)
	}
	_, _ = os.Stdout.WriteString("from-stdout\n")
	_, _ = os.Stderr.WriteString("from-stderr\n")
}

func TestIsolateStdioPreventsNonblockLeak(t *testing.T) {
	// Point both stdout and stderr at the same underlying *os.File to simulate
	// a shared PTY or shell 2>&1 redirection.
	sharedPath := filepath.Join(t.TempDir(), "shared.log")
	shared, err := os.OpenFile(sharedPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer shared.Close()

	fcOut, fcErr := isolateStdio(shared, shared)
	cmd := exec.Command(os.Args[0], "-test.run=^TestStdioIsolationHelper$")
	cmd.Env = append(os.Environ(), "MOAT_TEST_NONBLOCK_HELPER=1")
	cmd.Stdout = fcOut
	cmd.Stderr = fcErr

	if err := cmd.Run(); err != nil {
		raw, _ := os.ReadFile(sharedPath)
		t.Fatalf("helper failed: %v\noutput:\n%s", err, raw)
	}

	parentFlags, err := unix.FcntlInt(shared.Fd(), unix.F_GETFL, 0)
	if err != nil {
		t.Fatalf("parent fcntl(F_GETFL): %v", err)
	}
	if parentFlags&unix.O_NONBLOCK != 0 {
		t.Fatalf("O_NONBLOCK leaked onto parent file descriptor (flags=0x%x)", parentFlags)
	}

	raw, err := os.ReadFile(sharedPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if !strings.Contains(got, "from-stdout\n") || !strings.Contains(got, "from-stderr\n") {
		t.Fatalf("expected both stdout and stderr in output, got %q", got)
	}
}
