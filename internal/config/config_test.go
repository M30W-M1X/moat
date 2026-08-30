package config

import (
	"os"
	"path/filepath"
	"testing"
)

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
