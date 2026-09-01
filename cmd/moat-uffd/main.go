// Command moat-uffd is a userfaultfd page-fault handler for Firecracker
// snapshot restore. Firecracker connects to a Unix socket, passes the
// userfaultfd descriptor and the guest memory layout, and from then on every
// guest page fault is serviced here from the snapshot memory file. Restore
// returns as soon as the CPU state loads; pages stream in on demand.
//
//	moat-uffd --socket /run/x/uffd.sock --mem /run/x/snap/mem.bin
//
// Protocol matches Firecracker v1.16.x: the first socket message is a JSON
// array of {base_host_virt_addr,size,offset,page_size} mappings, with the uffd
// fd in the SCM_RIGHTS control data.
package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// From <linux/userfaultfd.h> on x86_64. _IOWR(0xAA, nr, sizeof(struct)).
const (
	uffdEventPagefault = 0x12
	uffdEventRemove    = 0x9

	// _IOWR(0xAA, 0x03, sizeof(struct uffdio_copy)=40)
	uffdioCopy = 0xc028aa03
	// _IOWR(0xAA, 0x04, sizeof(struct uffdio_zeropage)=32)
	uffdioZeropage = 0xc020aa04
)

// version is the build version. The release build sets it with -ldflags -X.
var version = "dev"

// uffdioCopyReq mirrors struct uffdio_copy.
type uffdioCopyReq struct {
	Dst, Src, Len, Mode uint64
	Copy                int64
}

// uffdioZeropageReq mirrors struct uffdio_zeropage.
type uffdioZeropageReq struct {
	Start, RangeLen uint64
	Mode            uint64
	Zeropage        int64
}

// mapping is one guest memory region, as Firecracker serialises it.
type mapping struct {
	BaseHostVirtAddr uint64 `json:"base_host_virt_addr"`
	Size             uint64 `json:"size"`
	Offset           uint64 `json:"offset"`
	PageSize         uint64 `json:"page_size"`
}

func (m mapping) contains(addr uint64) bool {
	return addr >= m.BaseHostVirtAddr && addr < m.BaseHostVirtAddr+m.Size
}

func main() {
	sockPath := flag.String("socket", "", "unix socket path Firecracker connects to")
	memPath := flag.String("mem", "", "snapshot memory file")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("moat-uffd", version)
		return
	}
	if *sockPath == "" || *memPath == "" {
		fmt.Fprintln(os.Stderr, "usage: moat-uffd --socket PATH --mem PATH")
		os.Exit(2)
	}
	if err := run(*sockPath, *memPath); err != nil {
		fmt.Fprintf(os.Stderr, "moat-uffd: %v\n", err)
		os.Exit(1)
	}
}

func run(sockPath, memPath string) error {
	_ = os.Remove(sockPath)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sockPath, Net: "unix"})
	if err != nil {
		return fmt.Errorf("listen %s: %w", sockPath, err)
	}
	defer ln.Close()
	defer os.Remove(sockPath)

	memFile, err := os.Open(memPath)
	if err != nil {
		return fmt.Errorf("open mem file: %w", err)
	}
	defer memFile.Close()
	fi, err := memFile.Stat()
	if err != nil {
		return err
	}
	backing, err := unix.Mmap(int(memFile.Fd()), 0, int(fi.Size()), unix.PROT_READ, unix.MAP_PRIVATE)
	if err != nil {
		return fmt.Errorf("mmap mem file: %w", err)
	}
	defer unix.Munmap(backing)
	backingBase := uint64(uintptr(unsafe.Pointer(&backing[0])))

	fmt.Println("moat-uffd: ready")

	conn, err := ln.AcceptUnix()
	if err != nil {
		return fmt.Errorf("accept: %w", err)
	}
	defer conn.Close()

	uffd, maps, err := recvSetup(conn)
	if err != nil {
		return err
	}
	defer unix.Close(uffd)
	if len(maps) == 0 {
		return fmt.Errorf("no memory mappings received")
	}
	pageSize := maps[0].PageSize
	fmt.Printf("moat-uffd: serving %d region(s), page size %d\n", len(maps), pageSize)

	return serve(uffd, backing, backingBase, maps, pageSize)
}

// recvSetup reads the first message: the JSON mapping array in the body and the
// userfaultfd descriptor in the ancillary data.
func recvSetup(conn *net.UnixConn) (int, []mapping, error) {
	buf := make([]byte, 64*1024)
	oob := make([]byte, unix.CmsgSpace(4))
	n, oobn, _, _, err := conn.ReadMsgUnix(buf, oob)
	if err != nil {
		return -1, nil, fmt.Errorf("read setup message: %w", err)
	}
	scms, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(scms) == 0 {
		return -1, nil, fmt.Errorf("parse control message: %w", err)
	}
	fds, err := unix.ParseUnixRights(&scms[0])
	if err != nil || len(fds) == 0 {
		return -1, nil, fmt.Errorf("no uffd descriptor in control message")
	}
	var maps []mapping
	if err := json.Unmarshal(buf[:n], &maps); err != nil {
		return -1, nil, fmt.Errorf("decode mappings %q: %w", buf[:n], err)
	}
	return fds[0], maps, nil
}

// serve reads userfaultfd events until the descriptor closes.
func serve(uffd int, backing []byte, backingBase uint64, maps []mapping, pageSize uint64) error {
	_ = backing             // keep the mmap alive for the loop
	msg := make([]byte, 32) // sizeof(struct uffd_msg)
	for {
		if _, err := unix.Read(uffd, msg); err != nil {
			if err == unix.EINTR {
				continue
			}
			return nil // descriptor closed: the VM is gone
		}
		switch msg[0] {
		case uffdEventPagefault:
			addr := binary.LittleEndian.Uint64(msg[16:24])
			if err := copyPage(uffd, backingBase, maps, addr&^(pageSize-1), pageSize); err != nil {
				fmt.Fprintf(os.Stderr, "moat-uffd: fault %#x: %v\n", addr, err)
			}
		case uffdEventRemove:
			start := binary.LittleEndian.Uint64(msg[8:16])
			end := binary.LittleEndian.Uint64(msg[16:24])
			zeroRange(uffd, start, end-start)
		}
	}
}

// copyPage fills one faulting page from the backing file.
func copyPage(uffd int, backingBase uint64, maps []mapping, dst, pageSize uint64) error {
	for _, m := range maps {
		if !m.contains(dst) {
			continue
		}
		src := backingBase + m.Offset + (dst - m.BaseHostVirtAddr)
		req := uffdioCopyReq{Dst: dst, Src: src, Len: pageSize}
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(uffd), uffdioCopy, uintptr(unsafe.Pointer(&req)))
		if errno == 0 || errno == unix.EEXIST {
			return nil
		}
		return fmt.Errorf("UFFDIO_COPY: %v (copy=%d)", errno, req.Copy)
	}
	// Address outside every region: hand back a zero page so the guest does not
	// hang on the fault.
	return zeroRange(uffd, dst, pageSize)
}

func zeroRange(uffd int, start, length uint64) error {
	req := uffdioZeropageReq{Start: start, RangeLen: length}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(uffd), uffdioZeropage, uintptr(unsafe.Pointer(&req)))
	if errno != 0 && errno != unix.EEXIST {
		return fmt.Errorf("UFFDIO_ZEROPAGE: %v", errno)
	}
	return nil
}
