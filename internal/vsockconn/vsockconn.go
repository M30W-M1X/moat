// Package vsockconn dials a guest vsock listener from the host through
// Firecracker's vsock multiplexing Unix socket.
//
// Protocol (host -> guest), per Firecracker's vsock docs: connect to the
// host-side UDS, send "CONNECT <port>\n", read the "OK <assigned_port>\n"
// reply, then the connection is a raw byte stream to the guest listener.
package vsockconn

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// Dial opens a stream to the guest process listening on vsock port.
// udsPath is config.Machine.VsockUDSPath().
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

	// Clear the deadline; the caller manages timeouts from here.
	_ = conn.SetDeadline(time.Time{})
	return &bufferedConn{Conn: conn, r: br}, nil
}

// WaitReady dials the guest port repeatedly until it answers or the context is
// done. Used right after boot while the guest agent is still coming up.
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

// bufferedConn preserves bytes that bufio read past the CONNECT reply.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }
