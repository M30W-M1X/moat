// Package vsockconn connects to a guest vsock listener from the host. It goes
// through the Firecracker vsock multiplex Unix socket.
//
// Protocol from host to guest, from the Firecracker vsock documentation:
//
//  1. Connect to the host-side Unix socket.
//  2. Send "CONNECT <port>\n".
//  3. Read the "OK <assigned_port>\n" reply.
//
// After step 3 the connection is a raw byte stream to the guest listener.
package vsockconn

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// Dial opens a byte stream to the guest process that listens on the given vsock
// port. udsPath is the value from config.Machine.VsockUDSPath.
func Dial(ctx context.Context, udsPath string, port uint32) (net.Conn, error) {
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "unix", udsPath)
	if err != nil {
		return nil, fmt.Errorf("dial vsock uds %s: %w", udsPath, err)
	}

	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		conn.Close()
		return nil, fmt.Errorf("write CONNECT: %w", err)
	}

	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read CONNECT reply: %w", err)
	}
	if !strings.HasPrefix(line, "OK ") {
		conn.Close()
		return nil, fmt.Errorf("guest refused vsock connect on port %d: %q", port, strings.TrimSpace(line))
	}

	// Clear the deadline. The caller controls timeouts after this point.
	_ = conn.SetDeadline(time.Time{})
	return &bufferedConn{Conn: conn, r: br}, nil
}

// WaitReady connects to the guest port again and again until the guest answers
// or the context ends. Use it just after start, while the guest agent still
// starts.
func WaitReady(ctx context.Context, udsPath string, port uint32) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var last error
	for {
		dialCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
		conn, err := Dial(dialCtx, udsPath, port)
		cancel()
		if err == nil {
			conn.Close()
			return nil
		}
		last = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("guest vsock port %d not ready: %w (last: %v)", port, ctx.Err(), last)
		case <-ticker.C:
		}
	}
}

// bufferedConn keeps the bytes that the bufio.Reader read after the CONNECT
// reply.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }
