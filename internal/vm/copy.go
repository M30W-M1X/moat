package vm

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// CreateScratchExt4 creates a sparse ext4 scratch disk at path with size
// sizeMiB. The guest agent mounts this drive as an overlayfs upper layer.
func CreateScratchExt4(path string, sizeMiB int64) error {
	if sizeMiB <= 0 {
		sizeMiB = 512
	}
	_ = os.Remove(path)
	out, err := exec.Command(
		"mkfs.ext4", "-q", "-F", "-L", "scratch",
		"-E", "lazy_itable_init=0,lazy_journal_init=0",
		path, strconv.FormatInt(sizeMiB, 10)+"M",
	).CombinedOutput()
	if err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("create scratch ext4: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// CopyRootFS writes a private copy of src to dst. It tries a copy-on-write
// reflink first, then a sparse copy, and falls back to a byte copy.
func CopyRootFS(src, dst string) error {
	if err := exec.Command("cp", "--reflink=auto", "--sparse=always", src, dst).Run(); err == nil {
		return nil
	}
	_ = os.Remove(dst)

	if err := exec.Command("cp", "--sparse=always", src, dst).Run(); err == nil {
		return nil
	}
	_ = os.Remove(dst)

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		_ = os.Remove(dst)
		return err
	}
	return out.Close()
}
