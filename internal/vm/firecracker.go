// Package vm is the Firecracker driver. It is a small wrapper over
// firecracker-go-sdk. It starts and stops one microVM that a config.Machine
// describes.
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

// Instance is a microVM. The microVM can be running or stopped.
type Instance struct {
	Machine config.Machine
	machine *firecracker.Machine
	log     *logrus.Logger
}

// Boot starts a Firecracker microVM for m. It returns after the guest kernel
// starts. The caller must call Stop.
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

	kernelArgs := m.KernelArgs
	cfg := firecracker.Config{
		SocketPath:      m.APISocketPath(),
		KernelImagePath: m.KernelImagePath,
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

	// Networking: run the VMM in the prepared namespace and give the guest a
	// static address through the kernel command line. netconf.Apply must have
	// built the namespace, TAP, and rules already.
	if m.Net != nil {
		cfg.NetNS = m.Net.NetnsPath
		cfg.NetworkInterfaces = []firecracker.NetworkInterface{{
			StaticConfiguration: &firecracker.StaticNetworkConfiguration{
				MacAddress:  m.Net.GuestMAC,
				HostDevName: m.Net.TapName,
			},
		}}
		kernelArgs += " " + m.Net.KernelIPArg()
	}
	cfg.KernelArgs = kernelArgs

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
		// Unprivileged path. Run the firecracker binary directly, with no
		// chroot.
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

// Stop requests a guest shutdown, then stops the VMM. You can call Stop more
// than once.
func (in *Instance) Stop(ctx context.Context) error {
	if in == nil || in.machine == nil {
		return nil
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

	if err := in.machine.StopVMM(); err != nil {
		return fmt.Errorf("stop vmm: %w", err)
	}
	in.machine = nil
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
