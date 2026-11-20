// Command guest-agent is the agent that runs inside the VM. In Phase 2 it runs
// as PID 1 (init=/sbin/guest-agent) and serves the agent.v1.Agent gRPC service
// over vsock: run a process and stream its output, and move files in and out of
// the VM.
//
// The host reaches this service through Firecracker's vsock multiplex socket.
// See proto/agent/v1/agent.proto and internal/agentclient.
//
// Build a static binary: CGO_ENABLED=0 go build -o guest-agent ./guest/agent
package main

import (
	"fmt"
	"os"
	"runtime"
	"syscall"

	"google.golang.org/grpc"

	agentv1 "github.com/M30W-M1X/moat/gen/agent/v1"
	"github.com/mdlayher/vsock"
)

// vsockPort must match internal/config.VsockPort.
const vsockPort = 5252

func main() {
	isPID1 := os.Getpid() == 1
	logf("guest-agent starting (pid=%d, pid1=%v, goarch=%s)", os.Getpid(), isPID1, runtime.GOARCH)

	if isPID1 {
		mountPseudoFS()
		ensureBaseEnv()
		ensureResolvConf()
		startZombieReaper()
		// As PID 1 this process must never return. The kernel panics if init
		// exits.
		defer haltForever("main returned")
	}

	// Bind to VMADDR_CID_ANY. The guest has only one relevant CID.
	l, err := vsock.Listen(vsockPort, nil)
	if err != nil {
		logf("FATAL: listen vsock port %d: %v", vsockPort, err)
		if isPID1 {
			haltForever("vsock listen failed")
		}
		os.Exit(1)
	}
	logf("serving agent.v1.Agent on vsock port %d", vsockPort)

	srv := grpc.NewServer(
		grpc.MaxRecvMsgSize(16<<20),
		grpc.MaxSendMsgSize(16<<20),
	)
	agentv1.RegisterAgentServer(srv, &server{})

	err = srv.Serve(l)
	logf("grpc server stopped: %v", err)
	if isPID1 {
		haltForever("grpc server stopped")
	}
}

// ensureBaseEnv sets a usable PATH, HOME, and TERM when the kernel passed none
// to PID 1. exec.Command resolves a bare program name against this PATH, and
// child processes inherit these values.
func ensureBaseEnv() {
	if os.Getenv("PATH") == "" {
		_ = os.Setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	if os.Getenv("HOME") == "" {
		_ = os.Setenv("HOME", "/root")
	}
	if os.Getenv("TERM") == "" {
		_ = os.Setenv("TERM", "xterm")
	}
}

// ensureResolvConf writes a resolver list when the rootfs has none. The kernel
// ip= parameter sets the guest address but does not create this file.
func ensureResolvConf() {
	if _, err := os.Stat("/etc/resolv.conf"); err == nil {
		return
	}
	_ = os.MkdirAll("/etc", 0o755)
	_ = os.WriteFile("/etc/resolv.conf", []byte("nameserver 1.1.1.1\nnameserver 8.8.8.8\n"), 0o644)
}

func mountPseudoFS() {
	type m struct {
		src, dst, fs, data string
	}
	for _, x := range []m{
		{"proc", "/proc", "proc", ""},
		{"sysfs", "/sys", "sysfs", ""},
		{"devtmpfs", "/dev", "devtmpfs", ""},                            // the kernel usually mounts this already (CONFIG_DEVTMPFS_MOUNT)
		{"devpts", "/dev/pts", "devpts", "gid=5,mode=620,ptmxmode=666"}, // needed for PTY exec
		{"tmpfs", "/dev/shm", "tmpfs", "mode=1777"},
		{"tmpfs", "/run", "tmpfs", "mode=755"},
	} {
		_ = os.MkdirAll(x.dst, 0o755)
		err := syscall.Mount(x.src, x.dst, x.fs, 0, x.data)
		if err == nil || err == syscall.EBUSY {
			continue // EBUSY means the kernel mounted it already
		}
		logf("warn: mount %s on %s: %v", x.fs, x.dst, err)
	}
	_ = os.MkdirAll("/tmp", 0o1777)
	_ = os.Chmod("/tmp", 0o1777)
	// Some rootfs images have no /dev/ptmx node. Point it at the devpts one.
	if _, err := os.Stat("/dev/ptmx"); err != nil {
		_ = os.Symlink("pts/ptmx", "/dev/ptmx")
	}
}

func haltForever(reason string) {
	logf("halting: %s", reason)
	// Block this goroutine forever. The host stops the VMM to end the VM.
	select {}
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stdout, "guest-agent: "+format+"\n", args...)
}
