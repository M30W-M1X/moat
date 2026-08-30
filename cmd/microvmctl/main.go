// Command microvmctl is the Phase 1 control CLI for microvm_controller.
//
//	microvmctl demo    start a microVM, send a vsock message and read the reply, then stop the VM
//
// This is the Phase 1 exit test. One command starts a microVM. The command then
// confirms host-to-guest communication over vsock. The -jail flag is optional.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/M30W-M1X/microvm_controller/internal/config"
	"github.com/M30W-M1X/microvm_controller/internal/vm"
	"github.com/M30W-M1X/microvm_controller/internal/vsockconn"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "demo":
		os.Exit(runDemo(os.Args[2:]))
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
	fmt.Fprint(os.Stderr, `microvmctl - Phase 1 Firecracker microVM control

usage:
  microvmctl demo [flags]

demo flags:
  -id string       machine id (default: demo-<pid>)
  -kernel string   use this vmlinux path instead of the default
  -rootfs string   use this rootfs.ext4 path instead of the default
  -vcpus int       guest vCPUs (default 1)
  -mem int         guest memory MiB (default 256)
  -msg string      message to echo through the guest (default "ping")
  -jail            run Firecracker under the jailer (needs root)
  -keep            keep the VM running after the demo
`)
}

func runDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	id := fs.String("id", fmt.Sprintf("demo-%d", os.Getpid()), "machine id")
	kernel := fs.String("kernel", "", "use this vmlinux path instead of the default")
	rootfs := fs.String("rootfs", "", "use this rootfs.ext4 path instead of the default")
	vcpus := fs.Int64("vcpus", 1, "guest vCPUs")
	mem := fs.Int64("mem", 256, "guest memory MiB")
	msg := fs.String("msg", "ping", "message to echo through the guest")
	jail := fs.Bool("jail", false, "run under jailer (needs root)")
	keep := fs.Bool("keep", false, "keep the VM running after the demo")
	_ = fs.Parse(args)

	log := logrus.New()
	log.SetFormatter(&logrus.TextFormatter{FullTimestamp: true, TimestampFormat: time.RFC3339})

	m, err := config.NewMachine(*id)
	if err != nil {
		log.WithError(err).Error("build machine config")
		return 1
	}
	if *kernel != "" {
		m.KernelImagePath = *kernel
	}
	if *rootfs != "" {
		m.RootFSPath = *rootfs
	}
	m.VCPUs = *vcpus
	m.MemMiB = *mem
	m.Jail = *jail
	if *jail && os.Geteuid() != 0 {
		log.Warn("-jail is set but the process is not root. The jailer will probably fail.")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	log.WithFields(logrus.Fields{
		"id": m.ID, "kernel": m.KernelImagePath, "rootfs": m.RootFSPath,
		"vcpus": m.VCPUs, "mem_mib": m.MemMiB, "jail": m.Jail,
	}).Info("starting microVM")

	t0 := time.Now()
	inst, err := vm.Boot(ctx, m, log)
	if err != nil {
		log.WithError(err).Error("start failed")
		return 1
	}
	log.WithField("elapsed", time.Since(t0).Round(time.Millisecond)).Info("microVM started")

	rc := 0
	if err := echoRoundTrip(ctx, m, *msg, log); err != nil {
		log.WithError(err).Error("vsock echo failed")
		rc = 1
	}

	if *keep {
		log.Info("-keep is set. The VM stays running. Press Ctrl-C to exit. The VM keeps running after exit.")
		<-ctx.Done()
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := inst.Stop(shutCtx); err != nil {
		log.WithError(err).Warn("stop")
	} else {
		log.Info("microVM stopped")
	}
	return rc
}

// echoRoundTrip waits for the guest agent, sends msg over vsock, and checks that
// the guest sends the same text back.
func echoRoundTrip(ctx context.Context, m config.Machine, msg string, log *logrus.Logger) error {
	readyCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := vsockconn.WaitReady(readyCtx, m.VsockUDSPath(), config.VsockPort); err != nil {
		return fmt.Errorf("guest agent did not start: %w", err)
	}
	log.WithField("port", config.VsockPort).Info("guest vsock agent is ready")

	dialCtx, cancel2 := context.WithTimeout(ctx, 5*time.Second)
	defer cancel2()
	conn, err := vsockconn.Dial(dialCtx, m.VsockUDSPath(), config.VsockPort)
	if err != nil {
		return err
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(conn, "%s\n", msg); err != nil {
		return fmt.Errorf("send: %w", err)
	}

	buf := make([]byte, len(msg)+16)
	n, err := conn.Read(buf)
	if err != nil {
		return fmt.Errorf("read echo: %w", err)
	}
	got := strings.TrimRight(string(buf[:n]), "\r\n")
	if got != msg {
		return fmt.Errorf("echo mismatch: sent %q, got %q", msg, got)
	}
	log.WithFields(logrus.Fields{"sent": msg, "recv": got}).Info("vsock echo round-trip OK")
	return nil
}
