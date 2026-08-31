// Package imagelock loads and verifies images.lock, the record of the sha256
// digests and provenance for a Moat kernel and rootfs pair.
package imagelock

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// Schema is the only images.lock version this build understands.
const Schema = "moat.images.lock/v1"

// FileEntry describes the pinned kernel.
type FileEntry struct {
	Path       string `json:"path"`
	SHA256     string `json:"sha256"`
	CISnapshot string `json:"ci_snapshot"`
	Version    string `json:"version"`
	Source     string `json:"source"`
}

// RootfsEntry describes the built rootfs.
type RootfsEntry struct {
	Path        string `json:"path"`
	SHA256      string `json:"sha256"`
	SizeBytes   int64  `json:"size_bytes"`
	BaseImage   string `json:"base_image"`
	FSUUID      string `json:"fs_uuid"`
	MkfsVersion string `json:"mkfs_version"`
}

// FileRef is a path and its digest.
type FileRef struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// AgentEntry describes the guest agent that went into the rootfs.
type AgentEntry struct {
	SHA256 string `json:"sha256"`
	Go     string `json:"go"`
}

// Lock is the parsed images.lock.
type Lock struct {
	SchemaField     string      `json:"schema"`
	SourceDateEpoch int64       `json:"source_date_epoch"`
	Kernel          FileEntry   `json:"kernel"`
	Rootfs          RootfsEntry `json:"rootfs"`
	SBOM            FileRef     `json:"sbom"`
	GuestAgent      AgentEntry  `json:"guest_agent"`
	GeneratedBy     string      `json:"generated_by"`
	GeneratedAt     string      `json:"generated_at"`
}

// MismatchError reports that a file's digest does not match the lock.
type MismatchError struct {
	File string
	Want string
	Got  string
}

func (e *MismatchError) Error() string {
	return fmt.Sprintf("image %s digest mismatch: want %s, got %s", e.File, e.Want, e.Got)
}

// Load reads and validates a lock file. It rejects an unknown schema.
func Load(path string) (*Lock, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var l Lock
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if l.SchemaField != Schema {
		return nil, fmt.Errorf("%s: unknown schema %q, want %q", path, l.SchemaField, Schema)
	}
	return &l, nil
}

// Find returns the images.lock path for a rootfs, or "" when none exists. It
// looks in the rootfs directory first, then the parent directory.
func Find(rootfsPath string) string {
	dir := filepath.Dir(rootfsPath)
	beside := filepath.Join(dir, "images.lock")
	if fileExists(beside) {
		return beside
	}
	parent := filepath.Join(filepath.Dir(dir), "images.lock")
	if fileExists(parent) {
		return parent
	}
	return ""
}

// VerifyFiles hashes kernelPath and rootfsPath and compares them to the lock. A
// mismatch returns a *MismatchError that names the file and both digests.
func (l *Lock) VerifyFiles(kernelPath, rootfsPath string) error {
	for _, c := range []struct {
		name, path, want string
	}{
		{l.Kernel.Path, kernelPath, l.Kernel.SHA256},
		{l.Rootfs.Path, rootfsPath, l.Rootfs.SHA256},
	} {
		got, err := sha256File(c.path)
		if err != nil {
			return fmt.Errorf("hash %s: %w", c.name, err)
		}
		if got != c.want {
			return &MismatchError{File: c.name, Want: c.want, Got: got}
		}
	}
	return nil
}

// VerifySignature runs `cosign verify-blob` on the lock. It returns nil when
// identity is empty and cosign is not on PATH. It returns an error when
// identity is set and the check fails.
func (l *Lock) VerifySignature(lockPath, sigPath, identity, issuer string) error {
	if identity == "" {
		return nil
	}
	if _, err := exec.LookPath("cosign"); err != nil {
		return fmt.Errorf("cosign identity is set but cosign is not on PATH")
	}
	if !fileExists(sigPath) {
		return fmt.Errorf("cosign identity is set but %s is missing", sigPath)
	}
	args := []string{
		"verify-blob",
		"--signature", sigPath,
		"--certificate-identity", identity,
	}
	if issuer != "" {
		args = append(args, "--certificate-oidc-issuer", issuer)
	}
	args = append(args, lockPath)
	out, err := exec.Command("cosign", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("cosign verify-blob: %v: %s", err, out)
	}
	return nil
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
