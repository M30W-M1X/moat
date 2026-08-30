// Package config holds the paths and values that the Phase 1 controller needs to
// start one Firecracker microVM.
//
// Phase 1 scope: one command starts a microVM. The command then sends a vsock
// message and reads the reply. Later phases add networking, snapshots, warm
// pools, and the northbound API. See CLAUDE.md.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/M30W-M1X/microvm_controller/internal/netconf"
)

// VsockPort is the AF_VSOCK port. The guest agent listens on this port. The host
// connects to this port through the Firecracker vsock multiplex socket.
const VsockPort uint32 = 5252

// GuestCID is the context ID of the guest end of the vsock device. The host is
// always CID 2. CID 3 is the standard value for the first guest.
const GuestCID uint32 = 3

// Machine describes one microVM to start.
type Machine struct {
	// ID is a short identifier that is safe to use in a file name. It names the
	// runtime directory. Under the jailer it also names the chroot.
	ID string

	// KernelImagePath is an uncompressed vmlinux ELF file.
	KernelImagePath string
	// RootFSPath is a raw ext4 image. Firecracker attaches it as the root block
	// device.
	RootFSPath string
	// KernelArgs is the guest kernel command line.
	KernelArgs string

	// VCPUs and MemMiB set the guest size.
	VCPUs  int64
	MemMiB int64

	// RunDir holds the runtime state for this VM: the API socket, the vsock
	// multiplex socket, and the log file.
	RunDir string

	// Jail starts Firecracker under the jailer. The jailer applies a chroot,
	// seccomp filters, cgroups, and dropped privileges. The jailer needs root.
	// Leave this false for an unprivileged local test.
	Jail bool
	// JailerUID and JailerGID are the unprivileged IDs that Firecracker changes
	// to under the jailer.
	JailerUID int
	JailerGID int

	// FirecrackerBin and JailerBin give the paths to the binaries. An empty
	// string means "find the binary on PATH".
	FirecrackerBin string
	JailerBin      string

	// Net, when set, runs this VM in a dedicated network namespace with a TAP
	// and nftables isolation rules. Build it with netconf.BuildPlan and apply
	// it with netconf.Apply before Boot. Networking needs privilege.
	Net *netconf.Plan
}

// DefaultKernelArgs is a minimal console setup. It runs the guest agent as
// PID 1.
//
// firecracker-go-sdk adds "root=/dev/vda", "rw" or "ro" (from the root drive's
// IsReadOnly field), and "pci=off" on its own. Do not repeat those values here.
//
// Do not add "noapic". The Firecracker virtio-mmio devices need the IO-APIC for
// interrupt delivery. Without the IO-APIC the root block device does not work.
const DefaultKernelArgs = "console=ttyS0 reboot=k panic=1 " +
	"i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd " +
	"init=/sbin/guest-agent"

// NewMachine returns a Machine that has the Phase 1 default values. It resolves
// the image paths against the build/artifacts directory in the repo when the
// caller does not set absolute paths.
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

// APISocketPath returns the path where the Firecracker REST API listens for this
// machine.
func (m Machine) APISocketPath() string {
	return filepath.Join(m.RunDir, "firecracker.socket")
}

// VsockUDSPath returns the host-side Unix socket that Firecracker uses to
// multiplex vsock connections.
//
// To reach the guest from the host: connect to this socket. Then write
// "CONNECT <port>\n".
func (m Machine) VsockUDSPath() string {
	return filepath.Join(m.RunDir, "vsock.sock")
}

// LogPath returns the Firecracker log file for this machine.
func (m Machine) LogPath() string {
	return filepath.Join(m.RunDir, "firecracker.log")
}

// Validate makes sure that the images exist and that the required fields are
// set.
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

// repoRoot searches upward from the working directory and from the executable
// for a go.mod file. This lets the demo run from any directory in the tree.
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
