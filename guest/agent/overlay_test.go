package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHasOverlayArg(t *testing.T) {
	dir := t.TempDir()
	on := filepath.Join(dir, "on")
	off := filepath.Join(dir, "off")
	_ = os.WriteFile(on, []byte("console=ttyS0 root=/dev/vda ro moat.overlay=1 init=/sbin/guest-agent\n"), 0o644)
	_ = os.WriteFile(off, []byte("console=ttyS0 root=/dev/vda rw init=/sbin/guest-agent\n"), 0o644)

	if !hasOverlayArg(on) {
		t.Fatal("expected moat.overlay=1 to be detected")
	}
	if hasOverlayArg(off) {
		t.Fatal("expected cmdline without moat.overlay=1 to return false")
	}
	if hasOverlayArg(filepath.Join(dir, "missing")) {
		t.Fatal("expected missing cmdline to return false")
	}
}
