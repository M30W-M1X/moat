// Package agentclient is the host-side client for the in-VM agent. It dials the
// agent.v1.Agent gRPC service over vsock, through Firecracker's vsock multiplex
// socket, and gives Go callers plain methods for Info, Exec, and file transfer.
package agentclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	agentv1 "github.com/M30W-M1X/moat/gen/agent/v1"
	"github.com/M30W-M1X/moat/internal/vsockconn"
)

const maxMsg = 16 << 20

// Client talks to one guest agent.
type Client struct {
	conn *grpc.ClientConn
	rpc  agentv1.AgentClient
}

// Dial opens a client for the agent that listens on the given vsock port.
// udsPath is the value from config.Machine.VsockUDSPath.
func Dial(udsPath string, port uint32) (*Client, error) {
	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		return vsockconn.Dial(ctx, udsPath, port)
	}
	cc, err := grpc.NewClient(
		"passthrough:///vsock",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dialer),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(maxMsg),
			grpc.MaxCallSendMsgSize(maxMsg),
		),
	)
	if err != nil {
		return nil, err
	}
	return &Client{conn: cc, rpc: agentv1.NewAgentClient(cc)}, nil
}

// Close shuts the client down.
func (c *Client) Close() error { return c.conn.Close() }

// Info returns basic facts about the guest and the agent.
func (c *Client) Info(ctx context.Context) (*agentv1.InfoResponse, error) {
	return c.rpc.Info(ctx, &agentv1.InfoRequest{})
}

// WaitReady calls Info again and again until the agent answers or ctx ends.
func (c *Client) WaitReady(ctx context.Context) error {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	var last error
	for {
		callCtx, cancel := context.WithTimeout(ctx, time.Second)
		_, err := c.Info(callCtx)
		cancel()
		if err == nil {
			return nil
		}
		last = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("agent not ready: %w (last: %v)", ctx.Err(), last)
		case <-t.C:
		}
	}
}

// ExecOptions describes one process to run in the guest.
type ExecOptions struct {
	Argv    []string
	Env     map[string]string
	Dir     string
	Stdin   []byte
	TTY     bool
	Cols    uint32
	Rows    uint32
	Timeout time.Duration // 0 means "no limit"
}

// ExecResult reports how a process ended.
type ExecResult struct {
	PID      int
	Code     int  // -1 when a signal ended the process
	Signal   int  // signal number, or 0
	TimedOut bool // the guest killed the process group after Timeout
}

// Exec runs a process in the guest. It writes the process stdout to stdout and
// the process stderr to stderr. Either writer may be nil. Exec returns after
// the process ends.
func (c *Client) Exec(ctx context.Context, o ExecOptions, stdout, stderr io.Writer) (ExecResult, error) {
	if len(o.Argv) == 0 {
		return ExecResult{}, errors.New("Argv is empty")
	}
	req := &agentv1.ExecRequest{
		Argv:      o.Argv,
		Env:       o.Env,
		Dir:       o.Dir,
		Stdin:     o.Stdin,
		Tty:       o.TTY,
		TtyCols:   o.Cols,
		TtyRows:   o.Rows,
		TimeoutMs: uint32(o.Timeout.Milliseconds()),
	}
	stream, err := c.rpc.Exec(ctx, req)
	if err != nil {
		return ExecResult{}, err
	}

	var res ExecResult
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return res, nil
		}
		if err != nil {
			return res, err
		}
		switch e := ev.GetEvent().(type) {
		case *agentv1.ExecEvent_Started:
			res.PID = int(e.Started.GetPid())
		case *agentv1.ExecEvent_Stdout:
			if stdout != nil {
				if _, werr := stdout.Write(e.Stdout); werr != nil {
					return res, werr
				}
			}
		case *agentv1.ExecEvent_Stderr:
			if stderr != nil {
				if _, werr := stderr.Write(e.Stderr); werr != nil {
					return res, werr
				}
			}
		case *agentv1.ExecEvent_Exit:
			res.Code = int(e.Exit.GetCode())
			res.Signal = int(e.Exit.GetSignal())
			res.TimedOut = e.Exit.GetTimedOut()
		}
	}
}

// WriteResult reports the outcome of a WriteFile call.
type WriteResult struct {
	BytesWritten uint64
	SHA256       string
}

// WriteFile streams r into the file at remotePath in the guest. mode of 0 means
// 0644.
func (c *Client) WriteFile(ctx context.Context, remotePath string, mode os.FileMode, r io.Reader) (WriteResult, error) {
	stream, err := c.rpc.WriteFile(ctx)
	if err != nil {
		return WriteResult{}, err
	}

	buf := make([]byte, 32<<10)
	first := true
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			chunk := &agentv1.WriteFileChunk{Data: buf[:n]}
			if first {
				chunk.Path = remotePath
				chunk.Mode = uint32(mode)
				first = false
			}
			if serr := stream.Send(chunk); serr != nil {
				return WriteResult{}, serr
			}
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return WriteResult{}, rerr
		}
	}
	if first {
		// r was empty; still send the header.
		if serr := stream.Send(&agentv1.WriteFileChunk{Path: remotePath, Mode: uint32(mode)}); serr != nil {
			return WriteResult{}, serr
		}
	}
	resp, err := stream.CloseAndRecv()
	if err != nil {
		return WriteResult{}, err
	}
	return WriteResult{BytesWritten: resp.GetBytesWritten(), SHA256: resp.GetSha256()}, nil
}

// ReadResult reports the outcome of a ReadFile call.
type ReadResult struct {
	Size   uint64
	SHA256 string
}

// ReadFile streams the file at remotePath in the guest into w.
func (c *Client) ReadFile(ctx context.Context, remotePath string, w io.Writer) (ReadResult, error) {
	stream, err := c.rpc.ReadFile(ctx, &agentv1.ReadFileRequest{Path: remotePath})
	if err != nil {
		return ReadResult{}, err
	}
	var res ReadResult
	for {
		chunk, rerr := stream.Recv()
		if errors.Is(rerr, io.EOF) {
			return res, nil
		}
		if rerr != nil {
			return res, rerr
		}
		if len(chunk.GetData()) > 0 {
			if _, werr := w.Write(chunk.GetData()); werr != nil {
				return res, werr
			}
		}
		if chunk.GetEof() {
			res.Size = chunk.GetSize()
			res.SHA256 = chunk.GetSha256()
		}
	}
}

// PutFile copies a local file into the guest.
func (c *Client) PutFile(ctx context.Context, localPath, remotePath string) (WriteResult, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return WriteResult{}, err
	}
	defer f.Close()
	mode := os.FileMode(0o644)
	if st, serr := f.Stat(); serr == nil {
		mode = st.Mode().Perm()
	}
	return c.WriteFile(ctx, remotePath, mode, f)
}

// GetFile copies a file out of the guest to a local path.
func (c *Client) GetFile(ctx context.Context, remotePath, localPath string) (ReadResult, error) {
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return ReadResult{}, err
	}
	f, err := os.Create(localPath)
	if err != nil {
		return ReadResult{}, err
	}
	defer f.Close()
	return c.ReadFile(ctx, remotePath, f)
}
