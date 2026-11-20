package imagelock

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// sha256OfString builds a want value the same way VerifyFiles computes it.
func sha256OfString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

const sample = `{
  "schema": "moat.images.lock/v1",
  "source_date_epoch": 1788134400,
  "kernel": {"path":"vmlinux","sha256":"KSUM","ci_snapshot":"snap","version":"6.1.182","source":"http://x"},
  "rootfs": {"path":"rootfs.ext4","sha256":"RSUM","size_bytes":268435456,"base_image":"alpine@sha256:x","fs_uuid":"u","mkfs_version":"mke2fs 1.47.1"},
  "sbom": {"path":"sbom.cdx.json","sha256":"SSUM"},
  "guest_agent": {"sha256":"ASUM","go":"go1.27.0"},
  "generated_by": "moat build pipeline",
  "generated_at": "2026-08-31T00:00:00Z"
}`

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "images.lock")
	writeFile(t, p, sample)
	l, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if l.SchemaField != "moat.images.lock/v1" || l.Kernel.SHA256 != "KSUM" ||
		l.Rootfs.SHA256 != "RSUM" || l.SBOM.SHA256 != "SSUM" || l.GuestAgent.Go != "go1.27.0" {
		t.Fatalf("bad parse: %+v", l)
	}
}

func TestLoadRejectsUnknownSchema(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "images.lock")
	writeFile(t, p, `{"schema":"moat.images.lock/v2"}`)
	if _, err := Load(p); err == nil {
		t.Fatal("want an error for an unknown schema")
	}
}

func TestLoadRejectsMalformed(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "images.lock")
	writeFile(t, p, `{not json`)
	if _, err := Load(p); err == nil {
		t.Fatal("want an error for malformed JSON")
	}
}

func TestFind(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "images")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	rootfs := filepath.Join(sub, "rootfs.ext4")
	writeFile(t, rootfs, "x")

	if got := Find(rootfs); got != "" {
		t.Fatalf("no lock anywhere, got %q", got)
	}
	parentLock := filepath.Join(dir, "images.lock")
	writeFile(t, parentLock, sample)
	if got := Find(rootfs); got != parentLock {
		t.Fatalf("parent lock: got %q want %q", got, parentLock)
	}
	besideLock := filepath.Join(sub, "images.lock")
	writeFile(t, besideLock, sample)
	if got := Find(rootfs); got != besideLock {
		t.Fatalf("beside lock should win: got %q want %q", got, besideLock)
	}
}

func TestVerifyFiles(t *testing.T) {
	dir := t.TempDir()
	kern := filepath.Join(dir, "vmlinux")
	root := filepath.Join(dir, "rootfs.ext4")
	writeFile(t, kern, "kernel-bytes")
	writeFile(t, root, "rootfs-bytes")

	l := &Lock{}
	l.Kernel.Path = "vmlinux"
	l.Kernel.SHA256 = sha256OfString("kernel-bytes")
	l.Rootfs.Path = "rootfs.ext4"
	l.Rootfs.SHA256 = sha256OfString("rootfs-bytes")
	if err := l.VerifyFiles(kern, root); err != nil {
		t.Fatalf("want pass, got %v", err)
	}

	writeFile(t, root, "rootfs-bytes-TAMPERED")
	err := l.VerifyFiles(kern, root)
	var me *MismatchError
	if !errors.As(err, &me) {
		t.Fatalf("want *MismatchError, got %v", err)
	}
	if me.File != "rootfs.ext4" || me.Want == "" || me.Got == "" || me.Want == me.Got {
		t.Fatalf("mismatch fields wrong: %+v", me)
	}
}

func TestVerifySignatureNoop(t *testing.T) {
	l := &Lock{}
	// cosign absent + identity empty -> nil.
	if err := l.VerifySignature("images.lock", "images.lock.sig", "", ""); err != nil {
		t.Fatalf("want nil, got %v", err)
	}
}

func TestCoversAndVerifyKernel(t *testing.T) {
	dir := t.TempDir()
	kern := filepath.Join(dir, "vmlinux")
	writeFile(t, kern, "kernel-bytes")

	l := &Lock{}
	l.Kernel.Path = "vmlinux"
	l.Kernel.SHA256 = sha256OfString("kernel-bytes")
	l.Rootfs.Path = "rootfs.ext4"

	if !l.CoversRootFS(filepath.Join(dir, "rootfs.ext4")) {
		t.Fatal("lock must cover matching rootfs basename")
	}
	if l.CoversRootFS(filepath.Join(dir, "python-rootfs.ext4")) {
		t.Fatal("lock must not cover a secondary template rootfs")
	}
	if !l.CoversKernel(kern) {
		t.Fatal("lock must cover matching kernel basename")
	}
	if err := l.VerifyKernel(kern); err != nil {
		t.Fatalf("VerifyKernel pass: %v", err)
	}
	writeFile(t, kern, "tampered")
	var me *MismatchError
	if err := l.VerifyKernel(kern); !errors.As(err, &me) {
		t.Fatalf("want *MismatchError, got %v", err)
	}
}
