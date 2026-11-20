package server

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/sirupsen/logrus"
)

func testSHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func writeTestLock(t *testing.T, dir, ksum, rsum string) {
	t.Helper()
	body := `{
  "schema": "moat.images.lock/v1",
  "source_date_epoch": 1788134400,
  "kernel": {"path":"vmlinux","sha256":"` + ksum + `","ci_snapshot":"s","version":"6.1.182","source":"http://x"},
  "rootfs": {"path":"rootfs.ext4","sha256":"` + rsum + `","size_bytes":1,"base_image":"alpine@sha256:x","fs_uuid":"u","mkfs_version":"m"},
  "sbom": {"path":"sbom.cdx.json","sha256":"deadbeef"},
  "guest_agent": {"sha256":"a","go":"go1.27.0"},
  "generated_by":"test","generated_at":"2026-08-31T00:00:00Z"
}`
	if err := os.WriteFile(filepath.Join(dir, "images.lock"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyImagesDropsFailedTemplateInEnforceMode(t *testing.T) {
	dirBad := t.TempDir()
	kBad := filepath.Join(dirBad, "vmlinux")
	rBad := filepath.Join(dirBad, "rootfs.ext4")
	_ = os.WriteFile(kBad, []byte("kern"), 0o644)
	_ = os.WriteFile(rBad, []byte("tampered-rootfs"), 0o644)
	writeTestLock(t, dirBad, testSHA256("kern"), testSHA256("original-rootfs"))

	dirGood := t.TempDir()
	kGood := filepath.Join(dirGood, "vmlinux")
	rGood := filepath.Join(dirGood, "rootfs.ext4")
	_ = os.WriteFile(kGood, []byte("kern"), 0o644)
	_ = os.WriteFile(rGood, []byte("good-rootfs"), 0o644)
	writeTestLock(t, dirGood, testSHA256("kern"), testSHA256("good-rootfs"))

	log := logrus.New()
	log.SetLevel(logrus.PanicLevel)
	srv, err := New(Config{
		Templates: []Template{
			{Name: "bad", Kernel: kBad, RootFS: rBad, VCPUs: 1, MemMiB: 128},
			{Name: "good", Kernel: kGood, RootFS: rGood, VCPUs: 1, MemMiB: 128},
		},
		Tokens:      map[string]string{"t": "s"},
		ImageVerify: "enforce",
	}, log)
	if err != nil {
		t.Fatalf("New should succeed when one template survives: %v", err)
	}
	if _, ok := srv.tmpl["bad"]; ok {
		t.Fatal("failed template must be removed from server map in enforce mode")
	}
	if _, ok := srv.tmpl["good"]; !ok {
		t.Fatal("verified template must remain usable")
	}
	if srv.defTmpl != "good" {
		t.Fatalf("defTmpl = %q, want good (promoted after bad was dropped)", srv.defTmpl)
	}
}

func TestVerifyImagesSecondaryTemplateBesideLock(t *testing.T) {
	dir := t.TempDir()
	kern := filepath.Join(dir, "vmlinux")
	defRoot := filepath.Join(dir, "rootfs.ext4")
	pyRoot := filepath.Join(dir, "python-rootfs.ext4")
	_ = os.WriteFile(kern, []byte("kern"), 0o644)
	_ = os.WriteFile(defRoot, []byte("default-rootfs"), 0o644)
	_ = os.WriteFile(pyRoot, []byte("python-rootfs"), 0o644)
	writeTestLock(t, dir, testSHA256("kern"), testSHA256("default-rootfs"))

	log := logrus.New()
	log.SetLevel(logrus.PanicLevel)
	srv, err := New(Config{
		Templates: []Template{
			{Name: "default", Kernel: kern, RootFS: defRoot, VCPUs: 1, MemMiB: 128},
			{Name: "python", Kernel: kern, RootFS: pyRoot, VCPUs: 1, MemMiB: 128},
		},
		Tokens:      map[string]string{"t": "s"},
		ImageVerify: "enforce",
	}, log)
	if err != nil {
		t.Fatalf("secondary template beside images.lock should stay usable: %v", err)
	}
	if _, ok := srv.tmpl["default"]; !ok {
		t.Fatal("default template must remain usable")
	}
	if _, ok := srv.tmpl["python"]; !ok {
		t.Fatal("secondary template must remain usable when kernel matches")
	}

	// Tampering the kernel must still reject the secondary template in enforce mode.
	_ = os.WriteFile(kern, []byte("tampered-kernel"), 0o644)
	if _, err := New(Config{
		Templates:   []Template{{Name: "python", Kernel: kern, RootFS: pyRoot, VCPUs: 1, MemMiB: 128}},
		Tokens:      map[string]string{"t": "s"},
		ImageVerify: "enforce",
	}, log); err == nil {
		t.Fatal("tampered kernel must still fail verification for a secondary template")
	}
}
