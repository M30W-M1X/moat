// Command microvmctl is the control CLI for microvm_controller.
//
//	microvmctl demo               start a microVM and check the agent over vsock
//	microvmctl run [flags] -- CMD start a microVM, run CMD in it, stream output, stop the VM
//
// Phase 2 adds run: it proves process execution and file transfer through the
// in-VM agent (agent.v1.Agent over vsock).
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/M30W-M1X/microvm_controller/internal/agentclient"
	"github.com/M30W-M1X/microvm_controller/internal/config"
	"github.com/M30W-M1X/microvm_controller/internal/vm"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "demo":
		os.Exit(runDemo(os.Args[2:]))
	case "run":
		os.Exit(runRun(os.Args[2:]))
	case "-h", "--help", "help":
		usage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `microvmctl - Firecracker microVM control

usage:
  microvmctl demo [flags]
  microvmctl run  [flags] -- CMD [ARG...]

demo flags:
  -id string       machine id (default: demo-<pid>)
  -kernel string   use this vmlinux path instead of the default
  -rootfs string   use this rootfs.ext4 path instead of the default
  -vcpus int       guest vCPUs (default 1)
  -mem int         guest memory MiB (default 256)
  -jail            run Firecracker under the jailer (needs root)
  -keep            keep the VM running after the command

run flags: the demo flags above, plus
  -dir string      working directory in the guest
  -env KEY=VALUE   set an environment variable (repeatable)
  -tty             run the command attached to a PTY
  -timeout dur     kill the command after this long (e.g. 30s); 0 means no limit
  -put LOCAL:REMOTE  copy a local file into the guest before the command (repeatable)
  -get REMOTE:LOCAL  copy a file out of the guest after the command (repeatable)
`)
}

// newLog returns a logger with a timestamped text format.
func newLog() *logrus.Logger {
	l := logrus.New()
	l.SetFormatter(&logrus.TextFormatter{FullTimestamp: true, TimestampFormat: time.RFC3339})
	return l
}

// machineFlags holds the flags that both commands share.
type machineFlags struct {
	id     *string
	kernel *string
	rootfs *string
	vcpus  *int64
	mem    *int64
	jail   *bool
	keep   *bool
}

func (mf machineFlags) apply(m *config.Machine, log *logrus.Logger) {
	if *mf.kernel != "" {
		m.KernelImagePath = *mf.kernel
	}
	if *mf.rootfs != "" {
		m.RootFSPath = *mf.rootfs
	}
	m.VCPUs = *mf.vcpus
	m.MemMiB = *mf.mem
	m.Jail = *mf.jail
	if *mf.jail && os.Geteuid() != 0 {
		log.Warn("-jail is set but the process is not root. The jailer will probably fail.")
	}
}

// bootMachine builds the config, starts the VM, and returns a ready agent
// client. The caller must call inst.Stop and cli.Close.
func bootMachine(ctx context.Context, m config.Machine, log *logrus.Logger) (*vm.Instance, *agentclient.Client, error) {
	log.WithFields(logrus.Fields{
		"id": m.ID, "vcpus": m.VCPUs, "mem_mib": m.MemMiB, "jail": m.Jail,
	}).Info("starting microVM")

	t0 := time.Now()
	inst, err := vm.Boot(ctx, m, log)
	if err != nil {
		return nil, nil, fmt.Errorf("start: %w", err)
	}
	log.WithField("elapsed", time.Since(t0).Round(time.Millisecond)).Info("microVM started")

	cli, err := agentclient.Dial(m.VsockUDSPath(), config.VsockPort)
	if err != nil {
		_ = inst.Stop(context.Background())
		return nil, nil, fmt.Errorf("dial agent: %w", err)
	}
	readyCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := cli.WaitReady(readyCtx); err != nil {
		cli.Close()
		_ = inst.Stop(context.Background())
		return nil, nil, fmt.Errorf("agent did not start: %w", err)
	}
	return inst, cli, nil
}

// stopMachine closes the client and stops the VM with a fresh timeout.
func stopMachine(inst *vm.Instance, cli *agentclient.Client, log *logrus.Logger) {
	if cli != nil {
		cli.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := inst.Stop(ctx); err != nil {
		log.WithError(err).Warn("stop")
		return
	}
	log.Info("microVM stopped")
}

// signalContext returns a context that ends on the first interrupt.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt)
}
