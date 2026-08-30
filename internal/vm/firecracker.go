// Package vm is the Firecracker driver: a thin wrapper over firecracker-go-sdk
// that boots and stops a single microVM described by a config.Machine.
package vm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	models "github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/sirupsen/logrus"

	"github.com/M30W-M1X/microvm_controller/internal/config"
)

// Instance is a running (or stopped) microVM.
type Instance struct {
	Machine config.Machine
	machine *firecracker.Machine
	log     *logrus.Logger
}

// Boot starts a Firecracker microVM for m and returns once the guest kernel has
// been handed control. The caller is responsible for calling Stop.
func Boot(ctx context.Context, m config.Machine, log *logrus.Logger) (*Instance, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(m.RunDir, 0o755); err != nil {
		return nil, fmt.Errorf("create run dir: %w", err)
	}

	fcBin, err := resolveBin(m.FirecrackerBin, "firecracker")
	if err != nil {
		return nil, err
	}

	cfg := firecracker.Config{
		SocketPath:      m.APISocketPath(),
		KernelImagePath: m.KernelImagePath,
		KernelArgs:      m.KernelArgs,
		LogPath:         m.LogPath(),
		LogLevel:        "Info",
		Drives: []models.Drive{{
			DriveID:      firecracker.String("rootfs"),
			PathOnHost:   firecracker.String(m.RootFSPath),
			IsRootDevice: firecracker.Bool(true),
			IsReadOnly:   firecracker.Bool(false),
		}},
		VsockDevices: []firecracker.VsockDevice{{
			ID:   "vsock0",
			Path: m.VsockUDSPath(),
			CID:  config.GuestCID,
		}},
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  firecracker.Int64(m.VCPUs),
			MemSizeMib: firecracker.Int64(m.MemMiB),
			Smt:        firecracker.Bool(false),
		},
	}

	opts := []firecracker.Opt{
		firecracker.WithLogger(logrus.NewEntry(log)),
	}

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
		// Unprivileged path: run the firecracker binary directly, no chroot.
		cmd := firecracker.VMCommandBuilder{}.
			WithBin(fcBin).
			WithSocketPath(cfg.SocketPath).
			WithStdout(os.Stdout).
			WithStderr(os.Stderr).
			Build(ctx)
		opts = append(opts, firecracker.WithProcessRunner(cmd))
	}

	machine, err := firecracker.NewMachine(ctx, cfg, opts...)
	if err != nil {
		return nil, fmt.Errorf("new machine: %w", err)
	}
	if err := machine.Start(ctx); err != nil {
		return nil, fmt.Errorf("start machine: %w", err)
	}

	return &Instance{Machine: m, machine: machine, log: log}, nil
}

// Stop asks the guest to shut down, then tears down the VMM. It is safe to call
// more than once.
func (in *Instance) Stop(ctx context.Context) error {
	if in == nil || in.machine == nil {
		return nil
	}
	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// Best-effort graceful shutdown (CtrlAltDel over the API); fall back to
	// killing the VMM.
	_ = in.machine.Shutdown(shutdownCtx)

	stopCtx, cancel2 := context.WithTimeout(ctx, 5*time.Second)
	defer cancel2()
	done := make(chan error, 1)
	go func() { done <- in.machine.Wait(stopCtx) }()

	select {
	case <-done:
	case <-stopCtx.Done():
	}

	if err := in.machine.StopVMM(); err != nil {
		return fmt.Errorf("stop vmm: %w", err)
	}
	in.machine = nil
	return nil
}

// resolveBin returns explicit if set, else looks up name on PATH.
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
