// This file adds a transparent mode to the egress proxy. A redirect rule in the
// sandbox network namespace sends guest TCP port 80 and 443 to this listener.
// The proxy recovers the pre-redirect destination with SO_ORIGINAL_DST, reads
// the target host name from the HTTP request line or the TLS SNI, applies the
// same Policy, and then splices the raw bytes through. It does not terminate
// TLS.
package egress

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// soOriginalDst is the getsockopt option that returns the pre-DNAT address of a
// redirected connection. golang.org/x/sys/unix does not define it.
const soOriginalDst = 80

// peekLimit caps how many leading bytes the proxy buffers to find the host
// name before it gives up and uses the numeric destination.
const peekLimit = 16384

// ServeTransparent listens on addr for redirected connections and returns the
// address it bound. It stops when ctx is done.
func ServeTransparent(ctx context.Context, addr string, p *Proxy) (string, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", err
	}
	tl := ln.(*net.TCPListener)
	go func() {
		<-ctx.Done()
		_ = tl.Close()
	}()
	go func() {
		for {
			c, err := tl.AcceptTCP()
			if err != nil {
				return
			}
			go p.serveTransparentConn(c)
		}
	}()
	return ln.Addr().String(), nil
}

// serveTransparentConn handles one redirected connection.
func (p *Proxy) serveTransparentConn(client *net.TCPConn) {
	defer client.Close()

	dst, err := originalDst(client)
	if err != nil {
		p.log.WithError(err).Warn("egress transparent: no original destination")
		return
	}

	_ = client.SetReadDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReaderSize(client, peekLimit)
	host := hostFromStream(br, dst)
	_ = client.SetReadDeadline(time.Time{})

	rule, allow, sandbox, token, clientIP := p.evalPolicy(client.RemoteAddr().String(), host)
	label := "allow"
	if !allow {
		label = "deny"
	}
	p.decision.WithLabelValues(label).Inc()
	p.log.WithFields(map[string]any{
		"host": host, "dst": dst.String(), "mode": "transparent", "decision": label,
	}).Info("egress")

	// Record the decision with via = "transparent". Build a minimal request so
	// recordEgressResolved can read the method and port.
	method := http.MethodGet
	if dst.Port() == 443 {
		method = http.MethodConnect
	}
	ar := (&http.Request{Method: method, Host: dst.String(), RemoteAddr: client.RemoteAddr().String()}).
		WithContext(context.Background())
	p.recordEgressResolved(ar, host, allow, "transparent", rule, sandbox, token, clientIP)

	if !allow {
		return
	}

	up, err := p.dial.DialContext(context.Background(), "tcp", dst.String())
	if err != nil {
		p.log.WithError(err).WithField("dst", dst.String()).Warn("egress transparent: dial upstream")
		return
	}
	defer up.Close()

	// Forward the bytes already pulled from the socket, then splice the rest.
	if n := br.Buffered(); n > 0 {
		head, _ := br.Peek(n)
		if _, err := up.Write(head); err != nil {
			return
		}
	}
	pipe(client, up)
}

// hostFromStream returns the target host name for the connection. It reads the
// TLS SNI for port 443 and the HTTP Host header otherwise. It falls back to the
// numeric address when it cannot find a name.
func hostFromStream(br *bufio.Reader, dst netip.AddrPort) string {
	var host string
	if dst.Port() == 443 {
		host = peekTLSServerName(br)
	} else {
		host = peekHTTPHost(br)
	}
	if host == "" {
		return dst.Addr().String()
	}
	return host
}

// originalDst recovers the pre-redirect destination of a connection that an
// nftables or iptables REDIRECT / DNAT rule sent to this listener.
func originalDst(c *net.TCPConn) (netip.AddrPort, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return netip.AddrPort{}, err
	}
	var sa unix.RawSockaddrInet4
	size := uint32(unsafe.Sizeof(sa))
	var callErr error
	ctrlErr := raw.Control(func(fd uintptr) {
		_, _, e := unix.Syscall6(unix.SYS_GETSOCKOPT, fd,
			uintptr(unix.IPPROTO_IP), uintptr(soOriginalDst),
			uintptr(unsafe.Pointer(&sa)), uintptr(unsafe.Pointer(&size)), 0)
		if e != 0 {
			callErr = e
		}
	})
	if ctrlErr != nil {
		return netip.AddrPort{}, ctrlErr
	}
	if callErr != nil {
		return netip.AddrPort{}, fmt.Errorf("getsockopt SO_ORIGINAL_DST: %w", callErr)
	}
	portBytes := (*[2]byte)(unsafe.Pointer(&sa.Port))[:]
	port := binary.BigEndian.Uint16(portBytes)
	return netip.AddrPortFrom(netip.AddrFrom4(sa.Addr), port), nil
}

// peekHTTPHost returns the Host header value from the buffered request head
// without consuming the bytes. It returns "" when it cannot read a full header
// block within peekLimit bytes.
func peekHTTPHost(br *bufio.Reader) string {
	for n := 512; n <= peekLimit; n += 512 {
		b, err := br.Peek(n)
		if i := bytes.Index(b, []byte("\r\n\r\n")); i >= 0 {
			return hostHeader(b[:i])
		}
		if err != nil {
			return hostHeader(b)
		}
	}
	return ""
}

func hostHeader(head []byte) string {
	for _, line := range bytes.Split(head, []byte("\r\n")) {
		if len(line) < 5 {
			continue
		}
		if strings.EqualFold(string(line[:5]), "host:") {
			h := strings.TrimSpace(string(line[5:]))
			if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.Contains(h[i:], "]") {
				h = h[:i]
			}
			return strings.ToLower(h)
		}
	}
	return ""
}

// peekTLSServerName returns the SNI host from a TLS ClientHello at the front of
// br, without consuming the bytes. It returns "" on any parse problem.
func peekTLSServerName(br *bufio.Reader) string {
	hdr, err := br.Peek(5)
	if err != nil || hdr[0] != 0x16 { // 0x16 = handshake record
		return ""
	}
	recLen := int(binary.BigEndian.Uint16(hdr[3:5]))
	if recLen < 4 || 5+recLen > peekLimit {
		return ""
	}
	buf, err := br.Peek(5 + recLen)
	if err != nil {
		return ""
	}
	name, perr := parseClientHelloSNI(buf[5 : 5+recLen])
	if perr != nil {
		return ""
	}
	return strings.ToLower(name)
}

// parseClientHelloSNI walks a TLS ClientHello handshake body and returns the
// first server_name extension host.
func parseClientHelloSNI(b []byte) (string, error) {
	r := cursor{b: b}
	if r.u8() != 0x01 { // 0x01 = ClientHello
		return "", errNotClientHello
	}
	r.skip(3)                // handshake length
	r.skip(2)                // client_version
	r.skip(32)               // random
	r.skip(int(r.u8()))      // session_id
	r.skip(int(r.u16()))     // cipher_suites
	r.skip(int(r.u8()))      // compression_methods
	extTotal := int(r.u16()) // extensions length
	if r.err != nil {
		return "", r.err
	}
	end := r.pos + extTotal
	for r.pos < end && r.err == nil {
		extType := r.u16()
		extLen := int(r.u16())
		if extType != 0x0000 { // 0x0000 = server_name
			r.skip(extLen)
			continue
		}
		r.skip(2)           // server_name_list length
		if r.u8() != 0x00 { // 0x00 = host_name
			return "", errNoHostName
		}
		host := r.bytes(int(r.u16()))
		if r.err != nil {
			return "", r.err
		}
		return string(host), nil
	}
	return "", errNoSNI
}

var (
	errNotClientHello = errors.New("not a ClientHello")
	errNoHostName     = errors.New("server_name entry is not host_name")
	errNoSNI          = errors.New("no server_name extension")
	errShort          = errors.New("record truncated")
)

// cursor reads big-endian fields from a byte slice and latches the first error.
type cursor struct {
	b   []byte
	pos int
	err error
}

func (c *cursor) need(n int) bool {
	if c.err != nil {
		return false
	}
	if c.pos+n > len(c.b) {
		c.err = errShort
		return false
	}
	return true
}

func (c *cursor) u8() int {
	if !c.need(1) {
		return 0
	}
	v := int(c.b[c.pos])
	c.pos++
	return v
}

func (c *cursor) u16() int {
	if !c.need(2) {
		return 0
	}
	v := int(binary.BigEndian.Uint16(c.b[c.pos : c.pos+2]))
	c.pos += 2
	return v
}

func (c *cursor) skip(n int) {
	if c.need(n) {
		c.pos += n
	}
}

func (c *cursor) bytes(n int) []byte {
	if !c.need(n) {
		return nil
	}
	v := c.b[c.pos : c.pos+n]
	c.pos += n
	return v
}
