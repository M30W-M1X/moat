package vsockconn

import (
	"bufio"
	"context"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeFirecrackerVsock stands in for Firecracker's host-side vsock mux socket:
// it accepts a Unix connection, expects "CONNECT <port>\n", replies
// "OK <port>\n", then echoes.
func fakeFirecrackerVsock(t *testing.T, wantPort string, accept bool) string {
	t.Helper()
	uds := filepath.Join(t.TempDir(), "vsock.sock")
	ln, err := net.Listen("unix", uds)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				line, err := br.ReadString('\n')
				if err != nil {
					return
				}
				if !strings.HasPrefix(line, "CONNECT "+wantPort) {
					c.Write([]byte("ERR bad port\n"))
					return
				}
				if !accept {
					c.Write([]byte("ERR refused\n"))
					return
				}
				c.Write([]byte("OK " + wantPort + "\n"))
				// echo the rest
				buf := make([]byte, 512)
				for {
					n, err := br.Read(buf)
					if n > 0 {
						c.Write(buf[:n])
					}
					if err != nil {
						return
					}
				}
			}(c)
		}
	}()
	return uds
}

func TestDialHandshakeAndStream(t *testing.T) {
	uds := fakeFirecrackerVsock(t, "5252", true)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := Dial(ctx, uds, 5252)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("hello\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 16)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := strings.TrimSpace(string(buf[:n])); got != "hello" {
		t.Fatalf("echo = %q, want hello", got)
	}
}

func TestDialRefused(t *testing.T) {
	uds := fakeFirecrackerVsock(t, "5252", false)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := Dial(ctx, uds, 5252); err == nil {
		t.Fatal("expected error when guest refuses the connection")
	}
}

func TestWaitReady(t *testing.T) {
	uds := fakeFirecrackerVsock(t, "5252", true)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := WaitReady(ctx, uds, 5252); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
}
