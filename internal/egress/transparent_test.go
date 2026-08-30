package egress

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestPeekHTTPHost(t *testing.T) {
	cases := map[string]string{
		"GET / HTTP/1.1\r\nHost: example.com\r\nUser-Agent: x\r\n\r\n":          "example.com",
		"GET / HTTP/1.1\r\nhost: API.Example.COM:8080\r\n\r\n":                  "api.example.com",
		"POST /x HTTP/1.1\r\nHost: files.pypi.org\r\nContent-Length: 0\r\n\r\n": "files.pypi.org",
	}
	for raw, want := range cases {
		br := bufio.NewReaderSize(strings.NewReader(raw), peekLimit)
		if got := peekHTTPHost(br); got != want {
			t.Errorf("peekHTTPHost(%q) = %q, want %q", raw, got, want)
		}
		// The bytes must still be readable in full afterwards.
		all, _ := io.ReadAll(br)
		if string(all) != raw {
			t.Errorf("stream was consumed: got %q", all)
		}
	}
}

func TestPeekHTTPHostNoHeader(t *testing.T) {
	br := bufio.NewReaderSize(strings.NewReader("not http at all"), peekLimit)
	if got := peekHTTPHost(br); got != "" {
		t.Errorf("want empty host, got %q", got)
	}
}

func TestPeekTLSServerName(t *testing.T) {
	// Drive a real TLS client handshake and capture its first flight.
	c, s := net.Pipe()
	defer c.Close()
	defer s.Close()

	go func() {
		_ = tls.Client(c, &tls.Config{
			ServerName:         "sni.example.com",
			InsecureSkipVerify: true,
		}).Handshake()
	}()

	_ = s.SetReadDeadline(time.Now().Add(2 * time.Second))
	br := bufio.NewReaderSize(s, peekLimit)
	if got := peekTLSServerName(br); got != "sni.example.com" {
		t.Fatalf("peekTLSServerName = %q, want %q", got, "sni.example.com")
	}
}

func TestPeekTLSServerNameNotTLS(t *testing.T) {
	br := bufio.NewReaderSize(bytes.NewReader([]byte("GET / HTTP/1.1\r\n\r\n")), peekLimit)
	if got := peekTLSServerName(br); got != "" {
		t.Errorf("want empty, got %q", got)
	}
}

func TestParseClientHelloSNITruncated(t *testing.T) {
	if _, err := parseClientHelloSNI([]byte{0x01, 0x00, 0x00}); err == nil {
		t.Error("expected an error on a truncated ClientHello")
	}
}
