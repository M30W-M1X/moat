package main

import (
	"net"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// TestRecvSetup feeds recvSetup the exact wire shape Firecracker uses: a JSON
// array of mapping descriptors in the body and a file descriptor in the
// SCM_RIGHTS control data. No privilege needed.
func TestRecvSetup(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	sender := os.NewFile(uintptr(fds[0]), "s")
	receiver := os.NewFile(uintptr(fds[1]), "r")
	defer sender.Close()
	defer receiver.Close()

	sc, err := net.FileConn(sender)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := net.FileConn(receiver)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	defer rc.Close()

	passed, err := os.Open(os.DevNull) // stands in for the userfaultfd
	if err != nil {
		t.Fatal(err)
	}
	defer passed.Close()

	body := `[{"base_host_virt_addr":140000000000,"size":268435456,"offset":0,"page_size":4096},` +
		`{"base_host_virt_addr":140300000000,"size":1048576,"offset":268435456,"page_size":4096}]`
	if _, _, err := sc.(*net.UnixConn).WriteMsgUnix([]byte(body), unix.UnixRights(int(passed.Fd())), nil); err != nil {
		t.Fatalf("WriteMsgUnix: %v", err)
	}

	uffd, maps, err := recvSetup(rc.(*net.UnixConn))
	if err != nil {
		t.Fatalf("recvSetup: %v", err)
	}
	defer unix.Close(uffd)

	if uffd < 0 {
		t.Errorf("uffd fd = %d, want a valid descriptor", uffd)
	}
	if len(maps) != 2 {
		t.Fatalf("got %d mappings, want 2", len(maps))
	}
	if maps[0].BaseHostVirtAddr != 140000000000 || maps[0].Size != 268435456 || maps[0].PageSize != 4096 {
		t.Errorf("mapping[0] = %+v", maps[0])
	}
	if maps[1].Offset != 268435456 {
		t.Errorf("mapping[1].Offset = %d, want 268435456", maps[1].Offset)
	}
	if !maps[0].contains(140000000000+4096) || maps[0].contains(140300000000) {
		t.Errorf("mapping[0].contains is wrong")
	}
}

// TestCopyPageSourceMath checks the serve_pf offset arithmetic:
// src = backingBase + region.offset + (dst - region.base).
func TestCopyPageSourceMath(t *testing.T) {
	maps := []mapping{
		{BaseHostVirtAddr: 0x1000_0000, Size: 0x1000_0000, Offset: 0, PageSize: 4096},
		{BaseHostVirtAddr: 0x3000_0000, Size: 0x0010_0000, Offset: 0x1000_0000, PageSize: 4096},
	}
	const backingBase = 0x7f00_0000_0000
	cases := []struct{ dst, wantSrc uint64 }{
		{0x1000_0000, backingBase},
		{0x1000_5000, backingBase + 0x5000},
		{0x3000_2000, backingBase + 0x1000_0000 + 0x2000},
	}
	for _, c := range cases {
		var got uint64
		for _, m := range maps {
			if m.contains(c.dst) {
				got = backingBase + m.Offset + (c.dst - m.BaseHostVirtAddr)
			}
		}
		if got != c.wantSrc {
			t.Errorf("dst %#x: src = %#x, want %#x", c.dst, got, c.wantSrc)
		}
	}
}
