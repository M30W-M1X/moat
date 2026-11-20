package vm

import (
	"io"
	"os"
	"os/exec"
)

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
