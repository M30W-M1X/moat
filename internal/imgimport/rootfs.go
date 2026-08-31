package imgimport

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const defaultSourceDateEpoch int64 = 1788134400 // 2026-08-31 00:00:00 UTC

// uuidFromName returns a stable filesystem UUID for a template name.
func uuidFromName(name string) string {
	h := sha256.Sum256([]byte("moat-template-uuid:" + name))
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

// seedFromName returns a stable directory hash seed for a template name.
func seedFromName(name string) string {
	h := sha256.Sum256([]byte("moat-template-seed:" + name))
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

// autoSizeBytes picks an ext4 size from the userland size.
func autoSizeBytes(treeBytes int64) int64 {
	const mib = 1 << 20
	n := treeBytes * 3 / 2
	n = (n + mib - 1) / mib * mib
	if n < 64*mib {
		n = 64 * mib
	}
	return n
}

// dirSizeBytes returns the apparent size of the tree under root.
func dirSizeBytes(root string) (int64, error) {
	out, err := exec.Command("du", "-sb", root).Output()
	if err != nil {
		return 0, fmt.Errorf("du %s: %v", root, err)
	}
	field, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\t")
	n, err := strconv.ParseInt(strings.TrimSpace(field), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse du output %q: %w", string(out), err)
	}
	return n, nil
}

// assembleRootfs adds the agent and the scaffolding to root, then writes an
// ext4 file at outPath. sizeBytes 0 means "size from the tree".
func assembleRootfs(root, agentPath, outPath, label string, sourceDateEpoch, sizeBytes int64) (int64, []string, error) {
	var warnings []string
	if sourceDateEpoch == 0 {
		sourceDateEpoch = defaultSourceDateEpoch
	}

	if err := installAgent(agentPath, filepath.Join(root, "sbin", "guest-agent")); err != nil {
		return 0, nil, err
	}
	for _, d := range []string{"proc", "sys", "dev", "tmp", "run", "work"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return 0, nil, err
		}
	}
	passwd := filepath.Join(root, "etc", "passwd")
	if _, err := os.Stat(passwd); err != nil {
		_ = os.MkdirAll(filepath.Join(root, "etc"), 0o755)
		_ = os.WriteFile(passwd, []byte("root:x:0:0:root:/root:/bin/sh\n"), 0o644)
	}
	// Lstat, not Stat: /bin/sh is a symlink to busybox in many images, and its
	// target does not resolve outside the guest.
	if _, err := os.Lstat(filepath.Join(root, "bin", "sh")); err != nil {
		warnings = append(warnings, "the image has no /bin/sh, so only absolute-path execs work")
	}

	epoch := strconv.FormatInt(sourceDateEpoch, 10)
	if out, err := exec.Command("find", root, "-exec", "touch", "--no-dereference", "--date=@"+epoch, "{}", "+").CombinedOutput(); err != nil {
		return 0, nil, fmt.Errorf("normalise timestamps: %v: %s", err, strings.TrimSpace(string(out)))
	}

	used, err := dirSizeBytes(root)
	if err != nil {
		return 0, nil, err
	}
	size := sizeBytes
	if size == 0 {
		size = autoSizeBytes(used)
	}

	tmp := outPath + ".tmp"
	_ = os.Remove(tmp)
	cmd := exec.Command("mkfs.ext4", "-q", "-F", "-L", label,
		"-U", uuidFromName(label),
		"-E", "hash_seed="+seedFromName(label)+",lazy_itable_init=0,lazy_journal_init=0",
		"-d", root, tmp, strconv.FormatInt(size/1024, 10)+"k")
	cmd.Env = append(os.Environ(), "E2FSPROGS_FAKE_TIME="+epoch)
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(tmp)
		return 0, warnings, fmt.Errorf("mkfs.ext4 (%d bytes may be too small, pass --size): %v: %s",
			size, err, strings.TrimSpace(string(out)))
	}
	if err := os.Rename(tmp, outPath); err != nil {
		return 0, warnings, err
	}
	return size, warnings, nil
}

// installAgent copies the static guest agent to dst with mode 0755.
func installAgent(src, dst string) error {
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("guest agent not found at %s (run: make agent, or pass --agent)", src)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := out.ReadFrom(in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
