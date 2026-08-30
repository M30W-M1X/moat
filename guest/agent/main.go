// Command guest-agent is the agent that runs inside the VM. In Phase 1 it runs
// as PID 1 (init=/sbin/guest-agent). It runs a vsock echo service. The echo
// service lets the host controller confirm two things: the VM starts, and the
// host and guest can communicate.
//
// Later phases add the full agent function: process execution with a PTY and
// streamed stdout and stderr, file read and write in chunks, a health
// heartbeat, and a clock resync after a snapshot resume. See CLAUDE.md.
//
// Build a static binary: CGO_ENABLED=0 go build -o guest-agent ./guest/agent
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/mdlayher/vsock"
)

// vsockPort must match internal/config.VsockPort.
const vsockPort = 5252

func main() {
	isPID1 := os.Getpid() == 1
	logf("guest-agent starting (pid=%d, pid1=%v, goarch=%s)", os.Getpid(), isPID1, runtime.GOARCH)

	if isPID1 {
		mountPseudoFS()
		// As PID 1 this process must never return. The kernel panics if init
		// exits.
		defer haltForever("main returned")
		go reapChildren()
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
	logf("listening on vsock port %d", vsockPort)

	for {
		conn, err := l.Accept()
		if err != nil {
			logf("accept: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go handle(conn)
	}
}

// handle sends each input line back without change. For the line "INFO" it sends
// a one-line status reply instead. The "INFO" line helps with manual tests.
func handle(conn io.ReadWriteCloser) {
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "INFO" {
			fmt.Fprintf(conn, "guest-agent ok uptime=%s\n", uptime())
			continue
		}
		if _, err := fmt.Fprintf(conn, "%s\n", line); err != nil {
			return
		}
	}
}

func mountPseudoFS() {
	type m struct{ src, dst, fs string }
	for _, x := range []m{
		{"proc", "/proc", "proc"},
		{"sysfs", "/sys", "sysfs"},
		{"devtmpfs", "/dev", "devtmpfs"}, // the kernel usually mounts this already (CONFIG_DEVTMPFS_MOUNT)
	} {
		_ = os.MkdirAll(x.dst, 0o755)
		err := syscall.Mount(x.src, x.dst, x.fs, 0, "")
		if err == nil || err == syscall.EBUSY {
			continue // EBUSY means the kernel mounted it already
		}
		logf("warn: mount %s on %s: %v", x.fs, x.dst, err)
	}
}

// reapChildren removes zombie child processes so they do not collect under
// PID 1.
func reapChildren() {
	ch := make(chan os.Signal, 8)
	signal.Notify(ch, syscall.SIGCHLD)
	for range ch {
		for {
			var ws syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
			if pid <= 0 || err != nil {
				break
			}
		}
	}
}

func uptime() string {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return "unknown"
	}
	var secs float64
	fmt.Sscanf(string(b), "%f", &secs)
	return time.Duration(secs * float64(time.Second)).Round(time.Second).String()
}

func haltForever(reason string) {
	logf("halting: %s", reason)
	// Block this goroutine forever. The host stops the VMM to end the VM.
	select {}
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stdout, "guest-agent: "+format+"\n", args...)
}
