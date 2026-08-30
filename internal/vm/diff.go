package vm

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// RebaseMemory builds a full guest memory file at outPath from a base memory
// file and a sparse diff memory file. It first copies basePath to outPath, then
// writes every populated region of diffPath over the same offset in outPath.
//
// The base and the diff must have the same length: the full guest memory size.
// RebaseMemory uses SEEK_DATA and SEEK_HOLE to find the populated regions, so
// the diff file must sit on a filesystem that reports sparse extents (ext4,
// xfs, btrfs, tmpfs).
func RebaseMemory(basePath, diffPath, outPath string) error {
	base, err := os.Open(basePath)
	if err != nil {
		return err
	}
	defer base.Close()
	baseInfo, err := base.Stat()
	if err != nil {
		return err
	}

	diff, err := os.Open(diffPath)
	if err != nil {
		return err
	}
	defer diff.Close()
	diffInfo, err := diff.Stat()
	if err != nil {
		return err
	}
	if diffInfo.Size() != baseInfo.Size() {
		return fmt.Errorf("size mismatch: base is %d bytes, diff is %d bytes",
			baseInfo.Size(), diffInfo.Size())
	}

	out, err := os.OpenFile(outPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()

	// Step 1: copy the whole base.
	if _, err := io.Copy(out, base); err != nil {
		return fmt.Errorf("copy base memory: %w", err)
	}

	// Step 2: overlay each data region of the diff.
	fd := int(diff.Fd())
	size := diffInfo.Size()
	var off int64
	for off < size {
		dataStart, err := unix.Seek(fd, off, unix.SEEK_DATA)
		if err != nil {
			if errors.Is(err, unix.ENXIO) {
				break // only holes remain before the end of the file
			}
			return fmt.Errorf("seek to data at %d: %w", off, err)
		}
		holeStart, err := unix.Seek(fd, dataStart, unix.SEEK_HOLE)
		if err != nil {
			return fmt.Errorf("seek to hole at %d: %w", dataStart, err)
		}
		region := io.NewSectionReader(diff, dataStart, holeStart-dataStart)
		if _, err := io.Copy(&offsetWriter{out, dataStart}, region); err != nil {
			return fmt.Errorf("copy diff region [%d,%d): %w", dataStart, holeStart, err)
		}
		off = holeStart
	}
	return out.Sync()
}

// offsetWriter turns WriteAt into a plain Write that starts at a fixed offset
// and advances with each call.
type offsetWriter struct {
	dst io.WriterAt
	off int64
}

func (w *offsetWriter) Write(p []byte) (int, error) {
	n, err := w.dst.WriteAt(p, w.off)
	w.off += int64(n)
	return n, err
}
