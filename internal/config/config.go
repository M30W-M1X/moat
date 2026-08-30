// Package config holds the small set of paths and tunables the Phase 1
// controller needs to boot a single Firecracker microVM.
//
// Phase 1 scope: one command boots a microVM and round-trips a vsock message.
// Networking, snapshots, warm pools, and the northbound API come in later phases
// (see CLAUDE.md).
package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// VsockPort is the AF_VSOCK port the guest agent listens on and the host dials
// through the Firecracker vsock multiplexing socket.
const VsockPort uint32 = 5252

// GuestCID is the context ID assigned to the guest end of the vsock device.
// Host is always CID 2; 3 is the conventional first guest.
const GuestCID uint32 = 3

// Machine describes a single microVM to boot.
type Machine struct {
	// ID is a short, filesystem-safe identifier. It names the runtime dir and,
	// under jailer, the chroot.
	ID string

	// KernelImagePath is an uncompressed vmlinux ELF.
	KernelImagePath string
	// RootFSPath is a raw ext4 image used as the root block device.
	RootFSPath string
	// KernelArgs is the guest kernel command line.
	KernelArgs string

	// VCPUs and MemMiB size the guest.
	VCPUs  int64
	MemMiB int64

	// RunDir holds per-VM runtime state (API socket, vsock mux socket, logs).
	RunDir string

	// Jail runs Firecracker under the jailer (chroot + seccomp + cgroups +
	// dropped privileges). Requires root; leave false for unprivileged local
	// bring-up.
	Jail bool
	// JailerUID/GID are the unprivileged ids Firecracker drops to under jailer.
	JailerUID int
	JailerGID int

	// FirecrackerBin and JailerBin locate the binaries; empty means "resolve
	// from PATH".
	FirecrackerBin string
	JailerBin      string
}

// DefaultKernelArgs is a minimal console setup with our guest agent as PID 1.
// firecracker-go-sdk injects "root=/dev/vda", "rw"/"ro" (from the root drive's
// IsReadOnly), and "pci=off" itself, so we do not repeat them here.
//
// Do NOT add "noapic": Firecracker's virtio-mmio devices need the IO-APIC for
// interrupt delivery, and disabling it makes the root block device unusable.
const DefaultKernelArgs = "console=ttyS0 reboot=k panic=1 " +
	"i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd " +
	"init=/sbin/guest-agent"

// NewMachine fills a Machine with Phase 1 defaults, resolving image paths
// against the repo's build/artifacts directory when not given absolute.
func NewMachine(id string) (Machine, error) {
	if id == "" {
		return Machine{}, fmt.Errorf("machine id must not be empty")
	}
	root, err := repoRoot()
	if err != nil {
		return Machine{}, err
	}
	art := filepath.Join(root, "build", "artifacts")
	runDir := filepath.Join(root, "run", id)

	return Machine{
		ID:              id,
		KernelImagePath: filepath.Join(art, "vmlinux"),
		RootFSPath:      filepath.Join(art, "rootfs.ext4"),
		KernelArgs:      DefaultKernelArgs,
		VCPUs:           1,
		MemMiB:          256,
		RunDir:          runDir,
		Jail:            false,
		JailerUID:       os.Getuid(),
		JailerGID:       os.Getgid(),
	}, nil
}

// APISocketPath is where the Firecracker REST API listens for this machine.
func (m Machine) APISocketPath() string {
	return filepath.Join(m.RunDir, "firecracker.socket")
}

// VsockUDSPath is the host-side Unix socket Firecracker uses to multiplex vsock
// connections. Host->guest: dial this and write "CONNECT <port>\n".
func (m Machine) VsockUDSPath() string {
	return filepath.Join(m.RunDir, "vsock.sock")
}

// LogPath is the Firecracker log file for this machine.
func (m Machine) LogPath() string {
	return filepath.Join(m.RunDir, "firecracker.log")
}

// Validate checks that the images exist and required fields are set.
func (m Machine) Validate() error {
	if m.ID == "" {
		return fmt.Errorf("id is empty")
	}
	for _, p := range []struct{ name, path string }{
		{"kernel", m.KernelImagePath},
		{"rootfs", m.RootFSPath},
	} {
		if p.path == "" {
			return fmt.Errorf("%s path is empty", p.name)
		}
		if _, err := os.Stat(p.path); err != nil {
			return fmt.Errorf("%s image %q: %w (run: make images)", p.name, p.path, err)
		}
	}
	if m.VCPUs < 1 {
		return fmt.Errorf("vcpus must be >= 1")
	}
	if m.MemMiB < 64 {
		return fmt.Errorf("mem must be >= 64 MiB")
	}
	return nil
}

// repoRoot walks up from the executable and the working directory looking for a
// go.mod, so the demo works from anywhere in the tree.
func repoRoot() (string, error) {
	candidates := []string{}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, wd)
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Dir(exe))
	}
	for _, start := range candidates {
		dir := start
		for {
			if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
				return dir, nil
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return "", fmt.Errorf("could not locate repo root (no go.mod above cwd or executable)")
}
