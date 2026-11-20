package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
)

// hasOverlayArg reports whether the kernel command line enables overlayfs over
// a read-only rootfs and /dev/vdb scratch disk.
func hasOverlayArg(cmdlinePath string) bool {
	b, err := os.ReadFile(cmdlinePath)
	if err != nil {
		return false
	}
	for _, f := range strings.Fields(string(b)) {
		if f == "moat.overlay=1" {
			return true
		}
	}
	return false
}

// mountOverlayRoot mounts /dev/vdb as the writable upper layer over the
// read-only root filesystem and pivots root into the merged view. It returns
// an error when the kernel lacks overlayfs or /dev/vdb is absent.
func mountOverlayRoot() error {
	// /run is already a writable tmpfs mounted by mountPseudoFS.
	const (
		scratchDir = "/run/moat-scratch"
		mergedDir  = "/run/moat-merged"
	)
	if err := os.MkdirAll(scratchDir, 0o755); err != nil {
		return fmt.Errorf("mkdir scratch: %w", err)
	}
	if err := syscall.Mount("/dev/vdb", scratchDir, "ext4", 0, ""); err != nil {
		return fmt.Errorf("mount /dev/vdb: %w", err)
	}

	upper := scratchDir + "/upper"
	work := scratchDir + "/work"
	for _, d := range []string{upper, work, mergedDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	opts := fmt.Sprintf("lowerdir=/,upperdir=%s,workdir=%s", upper, work)
	if err := syscall.Mount("overlay", mergedDir, "overlay", 0, opts); err != nil {
		return fmt.Errorf("mount overlay: %w", err)
	}

	// Move active pseudo-filesystems into the merged root before pivoting.
	for _, mnt := range []string{"/proc", "/sys", "/dev"} {
		dst := mergedDir + mnt
		_ = os.MkdirAll(dst, 0o755)
		_ = syscall.Mount(mnt, dst, "", syscall.MS_MOVE, "")
	}

	oldRoot := mergedDir + "/mnt/lower"
	if err := os.MkdirAll(oldRoot, 0o755); err != nil {
		return fmt.Errorf("mkdir old root: %w", err)
	}
	if err := syscall.PivotRoot(mergedDir, oldRoot); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := os.Chdir("/"); err != nil {
		return fmt.Errorf("chdir root: %w", err)
	}
	_ = syscall.Unmount("/mnt/lower", syscall.MNT_DETACH)
	_ = os.MkdirAll("/run", 0o755)
	_ = syscall.Mount("tmpfs", "/run", "tmpfs", 0, "mode=755")
	_ = os.MkdirAll("/tmp", 0o1777)
	_ = os.Chmod("/tmp", 0o1777)
	return nil
}
