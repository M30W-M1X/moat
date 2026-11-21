package vm

import (
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSnapshotMetaRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := "/tmp/moat-bench/snap1/vsock.sock"
	if err := writeSnapshotMeta(dir, "snap1", want); err != nil {
		t.Fatalf("writeSnapshotMeta: %v", err)
	}
	got := resolveSnapshotVsockPath(dir, filepath.Join(dir, "snapshot.bin"), "/fallback/vsock.sock")
	if got != want {
		t.Fatalf("resolveSnapshotVsockPath = %q, want %q", got, want)
	}
}

func TestExtractSnapshotVsockPathFallback(t *testing.T) {
	want := "/tmp/moat-bench/snap1/vsock.sock"

	t.Run("postcard varint encoding", func(t *testing.T) {
		dir := t.TempDir()
		var vbuf [binary.MaxVarintLen64]byte
		vn := binary.PutUvarint(vbuf[:], uint64(len(want)))
		buf := []byte{0x01, 0x05, 'v', 's', 'o', 'c', 'k'}
		buf = append(buf, vbuf[:vn]...)
		buf = append(buf, []byte(want)...)
		buf = append(buf, 0x00, 0x01)

		snapPath := filepath.Join(dir, "snapshot.bin")
		if err := os.WriteFile(snapPath, buf, 0o644); err != nil {
			t.Fatal(err)
		}
		if got := resolveSnapshotVsockPath(dir, snapPath, "/fallback/vsock.sock"); got != want {
			t.Fatalf("resolveSnapshotVsockPath varint = %q, want %q", got, want)
		}
	})

	t.Run("bincode uint64 encoding", func(t *testing.T) {
		dir := t.TempDir()
		buf := []byte{0x00, 0xff, 0x13, 0x37}
		var lenBuf [8]byte
		binary.LittleEndian.PutUint64(lenBuf[:], uint64(len(want)))
		buf = append(buf, lenBuf[:]...)
		buf = append(buf, []byte(want)...)
		buf = append(buf, 0x00, 0xde, 0xad, 0xbe, 0xef)

		snapPath := filepath.Join(dir, "snapshot.bin")
		if err := os.WriteFile(snapPath, buf, 0o644); err != nil {
			t.Fatal(err)
		}
		if got := resolveSnapshotVsockPath(dir, snapPath, "/fallback/vsock.sock"); got != want {
			t.Fatalf("resolveSnapshotVsockPath uint64 = %q, want %q", got, want)
		}
	})
}

func TestPrepareVsockPath(t *testing.T) {
	// Case 1: Stale socket file is removed cleanly.
	staleDir := t.TempDir()
	staleSock := filepath.Join(staleDir, "snap1", "vsock.sock")
	if err := os.MkdirAll(filepath.Dir(staleSock), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staleSock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareVsockPath(staleSock); err != nil {
		t.Fatalf("prepareVsockPath(stale): %v", err)
	}
	if _, err := os.Lstat(staleSock); !os.IsNotExist(err) {
		t.Fatalf("stale socket still exists: err=%v", err)
	}

	// Case 2: Active listening Unix socket must not be removed and returns an error.
	liveSock := filepath.Join(t.TempDir(), "vsock.sock")
	ln, err := net.Listen("unix", liveSock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	err = prepareVsockPath(liveSock)
	if err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("prepareVsockPath(live) = %v, want already-in-use error", err)
	}
	if _, statErr := os.Lstat(liveSock); statErr != nil {
		t.Fatalf("live socket was unexpectedly removed: %v", statErr)
	}
}
