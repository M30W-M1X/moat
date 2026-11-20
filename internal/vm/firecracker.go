// Package vm is the Firecracker driver. It is a small wrapper over
// firecracker-go-sdk. It starts, snapshots, restores, and stops one microVM
// that a config.Machine describes.
package vm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	models "github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	ops "github.com/firecracker-microvm/firecracker-go-sdk/client/operations"
	"github.com/sirupsen/logrus"

	"github.com/M30W-M1X/moat/internal/cgroup"
	"github.com/M30W-M1X/moat/internal/config"
)

// Instance is a microVM. The microVM can be running, paused, or stopped.
//
// Most instances are driven through firecracker-go-sdk (machine != nil). The
// UFFD restore path starts the VMM and the page-fault handler directly and
// fills the raw* fields instead.
type Instance struct {
	Machine config.Machine
	machine *firecracker.Machine
	log     *logrus.Logger
	pid     int

	// Raw (non-SDK) process handles, used only by RestoreUFFD.
	rawFC   *exec.Cmd
	rawUFFD *exec.Cmd

	// cgroupRemove tears down the per-VM cgroup once the VMM has exited.
	cgroupRemove func()
}

// PID returns the Firecracker process id, or 0 when it is not known.
func (in *Instance) PID() int { return in.pid }

// Boot starts a Firecracker microVM for m from a cold kernel and rootfs. It
// returns after the guest kernel starts. The caller must call Stop.
func Boot(ctx context.Context, m config.Machine, log *logrus.Logger) (*Instance, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	cfg, fcBin, err := baseConfig(m)
	if err != nil {
		return nil, err
	}
	cfg.KernelImagePath = m.KernelImagePath
	cfg.Drives = []models.Drive{{
		DriveID:      firecracker.String("rootfs"),
		PathOnHost:   firecracker.String(m.RootFSPath),
		IsRootDevice: firecracker.Bool(true),
		IsReadOnly:   firecracker.Bool(m.ReadOnlyRoot),
		RateLimiter:  bandwidthLimiter(m.DiskBWBytesPerSec),
	}}
	if m.ScratchPath != "" {
		cfg.Drives = append(cfg.Drives, models.Drive{
			DriveID:      firecracker.String("scratch"),
			PathOnHost:   firecracker.String(m.ScratchPath),
			IsRootDevice: firecracker.Bool(false),
			IsReadOnly:   firecracker.Bool(false),
			RateLimiter:  bandwidthLimiter(m.DiskBWBytesPerSec),
		})
	}
	if m.Net != nil && m.NetBWBytesPerSec > 0 {
		cfg.NetworkInterfaces[0].InRateLimiter = bandwidthLimiter(m.NetBWBytesPerSec)
		cfg.NetworkInterfaces[0].OutRateLimiter = bandwidthLimiter(m.NetBWBytesPerSec)
	}
	cfg.MachineCfg = models.MachineConfiguration{
		VcpuCount:  firecracker.Int64(m.VCPUs),
		MemSizeMib: firecracker.Int64(m.MemMiB),
		Smt:        firecracker.Bool(false),
	}

	kernelArgs := m.KernelArgs
	if m.ReadOnlyRoot && m.ScratchPath != "" {
		kernelArgs += " moat.overlay=1"
	}
	if m.Net != nil {
		kernelArgs += " " + m.Net.KernelIPArg()
	}
	cfg.KernelArgs = kernelArgs

	return startMachine(ctx, m, cfg, fcBin, log)
}

// startMachine applies the jailer or the unprivileged process runner, starts
// the VMM, and records the pid. extra opts run after the built-in ones.

// Restore starts a Firecracker microVM from a snapshot in snapDir (made by
// CreateSnapshot) and resumes it. It uses the file memory backend. The restore
// keeps the vsock socket path from the snapshot, so use the same machine id.
func Restore(ctx context.Context, m config.Machine, snapDir string, log *logrus.Logger) (*Instance, error) {
	return restoreFrom(ctx, m,
		filepath.Join(snapDir, "snapshot.bin"),
		filepath.Join(snapDir, "mem.bin"), false, log)
}

// RestoreForDiff restores like Restore, and also turns on dirty-page tracking
// in the restored guest. The caller can then run work and call
// CreateSnapshotDiff to write a diff against snapDir. Pause and resume on a
// running VMM break the guest vsock (Gotcha G-12), so a diff snapshot must
// start from a fresh restore like this one, not from a resumed instance.
func RestoreForDiff(ctx context.Context, m config.Machine, snapDir string, log *logrus.Logger) (*Instance, error) {
	return restoreFrom(ctx, m,
		filepath.Join(snapDir, "snapshot.bin"),
		filepath.Join(snapDir, "mem.bin"), true, log)
}

// RestoreDiff starts a microVM from a base snapshot plus a diff snapshot. It
// first rebases the sparse diff memory file onto a copy of the base memory
// file, then restores from the merged memory and the diff microVM state.
// baseDir and diffDir each hold a snapshot.bin and a mem.bin.
func RestoreDiff(ctx context.Context, m config.Machine, baseDir, diffDir string, log *logrus.Logger) (*Instance, error) {
	if err := os.MkdirAll(m.RunDir, 0o755); err != nil {
		return nil, fmt.Errorf("create run dir: %w", err)
	}
	merged := filepath.Join(m.RunDir, "merged-mem.bin")
	if err := RebaseMemory(
		filepath.Join(baseDir, "mem.bin"),
		filepath.Join(diffDir, "mem.bin"), merged); err != nil {
		return nil, fmt.Errorf("rebase diff memory: %w", err)
	}
	return restoreFrom(ctx, m, filepath.Join(diffDir, "snapshot.bin"), merged, false, log)
}

// restoreFrom loads snapPath (microVM state) and memPath (guest memory) with
// the file memory backend and resumes the guest. When trackDirty is set, the
// restored guest tracks dirty pages so a later diff snapshot works.
func restoreFrom(ctx context.Context, m config.Machine, snapPath, memPath string, trackDirty bool, log *logrus.Logger) (*Instance, error) {
	for _, p := range []string{snapPath, memPath} {
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("snapshot file %q: %w", p, err)
		}
	}
	cfg, fcBin, err := baseConfig(m)
	if err != nil {
		return nil, err
	}
	// The snapshot already holds the vsock and network devices. Re-adding them
	// fails: the SDK's load-snapshot handler tries `PUT /vsock` after the VM has
	// already resumed. Keep only NetNS so the VMM process joins the namespace.
	cfg.VsockDevices = nil
	cfg.NetworkInterfaces = nil

	// WithSnapshot also swaps the SDK handler chain to the load-snapshot path,
	// which skips the cold-boot kernel and drive checks.
	resume := firecracker.WithSnapshot(memPath, snapPath, func(sc *firecracker.SnapshotConfig) {
		sc.ResumeVM = true
		sc.EnableDiffSnapshots = trackDirty
	})
	return startMachine(ctx, m, cfg, fcBin, log, resume)
}

// baseConfig fills the parts of the Firecracker config that boot and restore
// share: the API socket, logging, the vsock device, and the network namespace.
func baseConfig(m config.Machine) (firecracker.Config, string, error) {
	if err := os.MkdirAll(m.RunDir, 0o755); err != nil {
		return firecracker.Config{}, "", fmt.Errorf("create run dir: %w", err)
	}
	// Clear sockets left by a previous crashed run. A live VM's sockets are
	// under a different run dir per id, and orphans are handled by Reconcile.
	_ = os.Remove(m.APISocketPath())
	_ = os.Remove(m.VsockUDSPath())
	fcBin, err := resolveBin(m.FirecrackerBin, "firecracker")
	if err != nil {
		return firecracker.Config{}, "", err
	}
	cfg := firecracker.Config{
		SocketPath: m.APISocketPath(),
		LogPath:    m.LogPath(),
		LogLevel:   "Info",
		VsockDevices: []firecracker.VsockDevice{{
			ID:   "vsock0",
			Path: m.VsockUDSPath(),
			CID:  config.GuestCID,
		}},
	}
	if m.Net != nil {
		cfg.NetNS = m.Net.NetnsPath
		cfg.NetworkInterfaces = []firecracker.NetworkInterface{{
			StaticConfiguration: &firecracker.StaticNetworkConfiguration{
				MacAddress:  m.Net.GuestMAC,
				HostDevName: m.Net.TapName,
			},
		}}
	}
	return cfg, fcBin, nil
}

func startMachine(ctx context.Context, m config.Machine, cfg firecracker.Config, fcBin string, log *logrus.Logger, extra ...firecracker.Opt) (*Instance, error) {
	opts := []firecracker.Opt{firecracker.WithLogger(logrus.NewEntry(log))}

	if m.Jail {
		jailerBin, err := resolveBin(m.JailerBin, "jailer")
		if err != nil {
			return nil, err
		}
		uid, gid := m.JailerUID, m.JailerGID
		cfg.JailerCfg = &firecracker.JailerConfig{
			UID:            &uid,
			GID:            &gid,
			ID:             m.ID,
			ExecFile:       fcBin,
			JailerBinary:   jailerBin,
			ChrootBaseDir:  filepath.Dir(m.RunDir),
			ChrootStrategy: firecracker.NewNaiveChrootStrategy(m.KernelImagePath),
			Stdout:         os.Stdout,
			Stderr:         os.Stderr,
		}
	} else {
		cmd := firecracker.VMCommandBuilder{}.
			WithBin(fcBin).
			WithSocketPath(cfg.SocketPath).
			WithStdout(os.Stdout).
			WithStderr(os.Stderr).
			Build(ctx)
		opts = append(opts, firecracker.WithProcessRunner(cmd))
	}
	opts = append(opts, extra...)

	machine, err := firecracker.NewMachine(ctx, cfg, opts...)
	if err != nil {
		return nil, fmt.Errorf("new machine: %w", err)
	}
	if err := machine.Start(ctx); err != nil {
		// Start can leave the VMM process running when a later configuration
		// step fails. Kill it so the run directory is reusable.
		_ = machine.StopVMM()
		return nil, fmt.Errorf("start machine: %w", err)
	}

	in := &Instance{Machine: m, machine: machine, log: log}
	if pid, err := machine.PID(); err == nil {
		in.pid = pid
		_ = os.WriteFile(m.PIDPath(), []byte(strconv.Itoa(pid)), 0o644)
	}

	if !m.Limits.Zero() && in.pid > 0 {
		remove, err := cgroup.Apply("mvm-"+m.ID, in.pid, m.Limits)
		if err != nil {
			_ = machine.StopVMM()
			return nil, fmt.Errorf("apply cgroup limits: %w", err)
		}
		in.cgroupRemove = remove
	}
	return in, nil
}

// bandwidthLimiter returns a Firecracker rate limiter that allows bytesPerSec
// sustained, with a one-second burst. It returns nil for a non-positive rate.
func bandwidthLimiter(bytesPerSec int64) *models.RateLimiter {
	if bytesPerSec <= 0 {
		return nil
	}
	return &models.RateLimiter{Bandwidth: &models.TokenBucket{
		Size:         firecracker.Int64(bytesPerSec),
		RefillTime:   firecracker.Int64(1000),
		OneTimeBurst: firecracker.Int64(bytesPerSec),
	}}
}

// Pause freezes the guest vCPUs.
func (in *Instance) Pause(ctx context.Context) error { return in.machine.PauseVM(ctx) }

// Resume unfreezes the guest vCPUs.
func (in *Instance) Resume(ctx context.Context) error { return in.machine.ResumeVM(ctx) }

// CreateSnapshot pauses the guest and writes a full snapshot into snapDir as
// snapshot.bin (device and CPU state) and mem.bin (guest memory). The guest
// stays paused; the caller resumes it or stops it.
func (in *Instance) CreateSnapshot(ctx context.Context, snapDir string) error {
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		return err
	}
	if err := in.machine.PauseVM(ctx); err != nil {
		return fmt.Errorf("pause: %w", err)
	}
	snapPath := filepath.Join(snapDir, "snapshot.bin")
	memPath := filepath.Join(snapDir, "mem.bin")
	if err := in.machine.CreateSnapshot(ctx, memPath, snapPath); err != nil {
		return fmt.Errorf("create snapshot: %w", err)
	}
	return nil
}

// CreateSnapshotDiff pauses the guest and writes a diff snapshot into snapDir.
// The microVM state file snapshot.bin is full; the memory file mem.bin is
// sparse and holds only the pages that changed since the restore. The guest
// must have started from RestoreForDiff. The guest stays paused.
func (in *Instance) CreateSnapshotDiff(ctx context.Context, snapDir string) error {
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		return err
	}
	if err := in.machine.PauseVM(ctx); err != nil {
		return fmt.Errorf("pause: %w", err)
	}
	snapPath := filepath.Join(snapDir, "snapshot.bin")
	memPath := filepath.Join(snapDir, "mem.bin")
	diff := func(p *ops.CreateSnapshotParams) {
		p.Body.SnapshotType = models.SnapshotCreateParamsSnapshotTypeDiff
	}
	if err := in.machine.CreateSnapshot(ctx, memPath, snapPath, diff); err != nil {
		return fmt.Errorf("create diff snapshot: %w", err)
	}
	return nil
}

// Stop requests a guest shutdown, then stops the VMM. You can call Stop more
// than once.
func (in *Instance) Stop(ctx context.Context) error {
	if in == nil {
		return nil
	}
	if in.machine == nil {
		return in.stopRaw()
	}
	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// Try a clean shutdown first. This sends CtrlAltDel through the API. If it
	// fails, StopVMM below ends the process.
	_ = in.machine.Shutdown(shutdownCtx)

	stopCtx, cancel2 := context.WithTimeout(ctx, 5*time.Second)
	defer cancel2()
	done := make(chan error, 1)
	go func() { done <- in.machine.Wait(stopCtx) }()

	select {
	case <-done:
	case <-stopCtx.Done():
	}

	err := in.machine.StopVMM()
	_ = os.Remove(in.Machine.PIDPath())
	in.machine = nil
	if in.cgroupRemove != nil {
		in.cgroupRemove()
		in.cgroupRemove = nil
	}
	if err != nil {
		return fmt.Errorf("stop vmm: %w", err)
	}
	return nil
}

// resolveBin returns the explicit path when the caller sets it. If not,
// resolveBin finds name on PATH.
func resolveBin(explicit, name string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("%s binary %q: %w", name, explicit, err)
		}
		return explicit, nil
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%s not found on PATH: %w", name, err)
	}
	return p, nil
}
