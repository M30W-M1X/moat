package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/M30W-M1X/microvm_controller/gen/agent/v1"
)

const agentVersion = "phase2"

// chunkSize is the payload size for streamed file and output data.
const chunkSize = 32 << 10

type server struct {
	agentv1.UnimplementedAgentServer
}

// Info returns basic facts about the guest and the agent.
func (s *server) Info(ctx context.Context, _ *agentv1.InfoRequest) (*agentv1.InfoResponse, error) {
	return &agentv1.InfoResponse{
		AgentVersion:  agentVersion,
		Pid:           int64(os.Getpid()),
		KernelRelease: kernelRelease(),
		UptimeSeconds: uptimeSeconds(),
	}, nil
}

// Exec runs one process and streams its output. It sends a Started event, then
// stdout and stderr chunks, then one Exit event.
func (s *server) Exec(req *agentv1.ExecRequest, stream agentv1.Agent_ExecServer) error {
	if len(req.GetArgv()) == 0 {
		return status.Error(codes.InvalidArgument, "argv is empty")
	}
	ctx := stream.Context()

	cmd := exec.CommandContext(ctx, req.Argv[0], req.Argv[1:]...)
	cmd.Env = append(os.Environ(), flattenEnv(req.GetEnv())...)
	if d := req.GetDir(); d != "" {
		cmd.Dir = d
	}

	send := newLockedSend(stream)

	var (
		timedOut bool
		waitErr  error
	)
	if req.GetTty() {
		timedOut, waitErr = runTTY(cmd, req, send)
	} else {
		timedOut, waitErr = runPipes(cmd, req, send)
	}

	// No ProcessState means the process never started. Report that as an error
	// rather than a fake exit code.
	if cmd.ProcessState == nil {
		return status.Errorf(codes.Internal, "start %q: %v", req.Argv[0], waitErr)
	}

	code, sig := waitStatus(cmd)
	return send(&agentv1.ExecEvent{Event: &agentv1.ExecEvent_Exit{Exit: &agentv1.Exit{
		Code:     int32(code),
		Signal:   int32(sig),
		TimedOut: timedOut,
	}}})
}

// runPipes runs the process with separate stdout and stderr pipes.
func runPipes(cmd *exec.Cmd, req *agentv1.ExecRequest, send sendFunc) (timedOut bool, waitErr error) {
	// Own process group so a timeout can end the whole tree.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return false, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return false, err
	}
	var stdin io.WriteCloser
	if len(req.GetStdin()) > 0 {
		if stdin, err = cmd.StdinPipe(); err != nil {
			return false, err
		}
	}

	if err := cmd.Start(); err != nil {
		return false, err
	}
	_ = send(startedEvent(cmd))

	if stdin != nil {
		_, _ = stdin.Write(req.GetStdin())
		_ = stdin.Close()
	}

	fired := armTimeout(cmd, req.GetTimeoutMs())

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); pump(stdout, func(b []byte) { _ = send(stdoutEvent(b)) }) }()
	go func() { defer wg.Done(); pump(stderr, func(b []byte) { _ = send(stderrEvent(b)) }) }()
	wg.Wait()

	return fired.Load(), cmd.Wait()
}

// runTTY runs the process attached to a PTY. stdout carries the merged streams.
func runTTY(cmd *exec.Cmd, req *agentv1.ExecRequest, send sendFunc) (timedOut bool, waitErr error) {
	ws := &pty.Winsize{Cols: uint16(req.GetTtyCols()), Rows: uint16(req.GetTtyRows())}
	if ws.Cols == 0 {
		ws.Cols = 80
	}
	if ws.Rows == 0 {
		ws.Rows = 24
	}
	// pty.StartWithSize sets Setsid and Setctty. The child is a group leader,
	// so a kill on -pid ends the whole group.
	ptmx, err := pty.StartWithSize(cmd, ws)
	if err != nil {
		return false, err
	}
	defer func() { _ = ptmx.Close() }()

	_ = send(startedEvent(cmd))

	if len(req.GetStdin()) > 0 {
		_, _ = ptmx.Write(req.GetStdin())
	}

	fired := armTimeout(cmd, req.GetTimeoutMs())

	// The PTY read returns an error when the process exits. That is expected.
	pump(ptmx, func(b []byte) { _ = send(stdoutEvent(b)) })

	return fired.Load(), cmd.Wait()
}

// WriteFile writes one file. The first chunk sets path and mode.
func (s *server) WriteFile(stream agentv1.Agent_WriteFileServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	path := first.GetPath()
	if path == "" {
		return status.Error(codes.InvalidArgument, "first chunk must set path")
	}
	mode := os.FileMode(first.GetMode())
	if mode == 0 {
		mode = 0o644
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return status.Errorf(codes.Internal, "make parent dir: %v", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return status.Errorf(codes.Internal, "open %s: %v", path, err)
	}
	defer f.Close()

	h := sha256.New()
	var total uint64
	write := func(b []byte) error {
		if len(b) == 0 {
			return nil
		}
		if _, werr := f.Write(b); werr != nil {
			return status.Errorf(codes.Internal, "write %s: %v", path, werr)
		}
		h.Write(b)
		total += uint64(len(b))
		return nil
	}
	if err := write(first.GetData()); err != nil {
		return err
	}
	for {
		chunk, rerr := stream.Recv()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return rerr
		}
		if err := write(chunk.GetData()); err != nil {
			return err
		}
	}
	if err := f.Sync(); err != nil {
		return status.Errorf(codes.Internal, "sync %s: %v", path, err)
	}
	return stream.SendAndClose(&agentv1.WriteFileResponse{
		BytesWritten: total,
		Sha256:       hex.EncodeToString(h.Sum(nil)),
	})
}

// ReadFile reads one file and streams it back. The final chunk sets eof, size,
// and sha256, and carries no data.
func (s *server) ReadFile(req *agentv1.ReadFileRequest, stream agentv1.Agent_ReadFileServer) error {
	f, err := os.Open(req.GetPath())
	if err != nil {
		return status.Errorf(codes.NotFound, "open %s: %v", req.GetPath(), err)
	}
	defer f.Close()

	h := sha256.New()
	var size uint64
	buf := make([]byte, chunkSize)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			size += uint64(n)
			if serr := stream.Send(&agentv1.FileChunk{Data: buf[:n]}); serr != nil {
				return serr
			}
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return status.Errorf(codes.Internal, "read %s: %v", req.GetPath(), rerr)
		}
	}
	return stream.Send(&agentv1.FileChunk{
		Eof:    true,
		Size:   size,
		Sha256: hex.EncodeToString(h.Sum(nil)),
	})
}

// ---- helpers ----

type sendFunc func(*agentv1.ExecEvent) error

// newLockedSend returns a sendFunc that is safe for concurrent use. A gRPC
// stream does not allow concurrent Send calls.
func newLockedSend(stream agentv1.Agent_ExecServer) sendFunc {
	var mu sync.Mutex
	return func(ev *agentv1.ExecEvent) error {
		mu.Lock()
		defer mu.Unlock()
		return stream.Send(ev)
	}
}

func startedEvent(cmd *exec.Cmd) *agentv1.ExecEvent {
	return &agentv1.ExecEvent{Event: &agentv1.ExecEvent_Started{Started: &agentv1.Started{Pid: int32(cmd.Process.Pid)}}}
}

func stdoutEvent(b []byte) *agentv1.ExecEvent {
	return &agentv1.ExecEvent{Event: &agentv1.ExecEvent_Stdout{Stdout: append([]byte(nil), b...)}}
}

func stderrEvent(b []byte) *agentv1.ExecEvent {
	return &agentv1.ExecEvent{Event: &agentv1.ExecEvent_Stderr{Stderr: append([]byte(nil), b...)}}
}

// pump reads r until it ends and calls emit for each block of bytes.
func pump(r io.Reader, emit func([]byte)) {
	buf := make([]byte, chunkSize)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			emit(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// armTimeout ends the process group after ms milliseconds. It returns a flag
// that becomes true when the timeout fired. ms of 0 means "no limit".
func armTimeout(cmd *exec.Cmd, ms uint32) *atomic.Bool {
	fired := &atomic.Bool{}
	if ms == 0 {
		return fired
	}
	time.AfterFunc(time.Duration(ms)*time.Millisecond, func() {
		fired.Store(true)
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	})
	return fired
}

// waitStatus reads the exit code and signal from a finished command. code is -1
// when a signal ended the process.
func waitStatus(cmd *exec.Cmd) (code int, sig int) {
	if cmd.ProcessState == nil {
		return -1, 0
	}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
		if ws.Signaled() {
			return -1, int(ws.Signal())
		}
		return ws.ExitStatus(), 0
	}
	return cmd.ProcessState.ExitCode(), 0
}

func flattenEnv(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

func uptimeSeconds() float64 {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	var secs float64
	_, _ = fmt.Sscanf(string(b), "%f", &secs)
	return secs
}

func kernelRelease() string {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(b))
}
