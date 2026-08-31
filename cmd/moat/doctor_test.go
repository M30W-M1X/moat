package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func writeF(t *testing.T, p, body string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func hexF(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestImageLockStatus(t *testing.T) {
	dir := t.TempDir()
	kern := filepath.Join(dir, "vmlinux")
	root := filepath.Join(dir, "rootfs.ext4")
	writeF(t, kern, "K")
	writeF(t, root, "R")

	// No lock -> WARN.
	if mark, _ := imageLockStatus(kern, root); mark != "WARN" {
		t.Fatalf("no lock: mark = %q, want WARN", mark)
	}

	lock := filepath.Join(dir, "images.lock")
	writeF(t, lock, `{"schema":"moat.images.lock/v1","kernel":{"path":"vmlinux","sha256":"`+hexF("K")+`"},"rootfs":{"path":"rootfs.ext4","sha256":"`+hexF("R")+`"},"sbom":{"path":"sbom.cdx.json","sha256":"abcd1234"}}`)

	// Matching lock -> PASS.
	if mark, _ := imageLockStatus(kern, root); mark != "PASS" {
		t.Fatalf("matching lock: mark = %q, want PASS", mark)
	}

	// Tampered rootfs -> FAIL.
	writeF(t, root, "R-TAMPERED")
	if mark, _ := imageLockStatus(kern, root); mark != "FAIL" {
		t.Fatalf("mismatch: mark = %q, want FAIL", mark)
	}
}
