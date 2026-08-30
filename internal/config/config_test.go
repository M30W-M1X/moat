package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuditDir(t *testing.T) {
	t.Setenv("MOAT_DATA", t.TempDir())
	got := AuditDir()
	if filepath.Base(got) != "audit" {
		t.Fatalf("AuditDir = %q, want it to end in /audit", got)
	}
	if !strings.HasPrefix(got, os.Getenv("MOAT_DATA")) {
		t.Fatalf("AuditDir %q not under MOAT_DATA", got)
	}
}

func TestNewMachineDefaults(t *testing.T) {
	m, err := NewMachine("unit-1")
	if err != nil {
		t.Fatalf("NewMachine: %v", err)
	}
	if m.ID != "unit-1" {
		t.Errorf("ID = %q, want unit-1", m.ID)
	}
	if m.VCPUs != 1 || m.MemMiB != 256 {
		t.Errorf("size = %d vcpu / %d MiB, want 1 / 256", m.VCPUs, m.MemMiB)
	}
	if m.Jail {
		t.Error("Jail should default to false")
	}
	if filepath.Base(m.APISocketPath()) != "firecracker.socket" {
		t.Errorf("APISocketPath = %q", m.APISocketPath())
	}
	if filepath.Base(m.VsockUDSPath()) != "vsock.sock" {
		t.Errorf("VsockUDSPath = %q", m.VsockUDSPath())
	}
}

func TestNewMachineRejectsEmptyID(t *testing.T) {
	if _, err := NewMachine(""); err == nil {
		t.Fatal("expected error for empty id")
	}
}

func TestValidate(t *testing.T) {
	dir := t.TempDir()
	kernel := filepath.Join(dir, "vmlinux")
	rootfs := filepath.Join(dir, "rootfs.ext4")

	m, err := NewMachine("unit-2")
	if err != nil {
		t.Fatalf("NewMachine: %v", err)
	}
	m.KernelImagePath = kernel
	m.RootFSPath = rootfs

	if err := m.Validate(); err == nil {
		t.Fatal("Validate should fail when images are missing")
	}

	for _, p := range []string{kernel, rootfs} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate should pass once images exist: %v", err)
	}

	m.VCPUs = 0
	if err := m.Validate(); err == nil {
		t.Error("Validate should reject vcpus < 1")
	}
	m.VCPUs = 1
	m.MemMiB = 8
	if err := m.Validate(); err == nil {
		t.Error("Validate should reject mem < 64 MiB")
	}
}

func TestDataDirFromEnv(t *testing.T) {
	t.Setenv("MOAT_DATA", "/opt/moat-data")
	if got := dataDir(); got != "/opt/moat-data" {
		t.Errorf("MOAT_DATA: dataDir() = %q, want /opt/moat-data", got)
	}

	t.Setenv("MOAT_DATA", "")
	t.Setenv("XDG_DATA_HOME", "/xdg")
	if got := dataDir(); got != "/xdg/moat" {
		t.Errorf("XDG_DATA_HOME: dataDir() = %q, want /xdg/moat", got)
	}

	t.Setenv("XDG_DATA_HOME", "")
	// With no env and (almost certainly) no ~/.local/share/moat/vmlinux, it
	// falls back to the system path.
	if got := dataDir(); got != "/var/lib/moat" {
		t.Skipf("dataDir() = %q — a ~/.local/share/moat install is present, skipping", got)
	}
}

func TestArtifactsDirUsesRepoInCheckout(t *testing.T) {
	// The test runs from a repo checkout, so ArtifactsDir must point into it,
	// not at an install path.
	if got := ArtifactsDir(); filepath.Base(got) != "artifacts" {
		t.Errorf("ArtifactsDir() = %q, want a .../build/artifacts path", got)
	}
}
