package vm

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"
)

const snapshotMetaFile = "meta.json"

type snapshotMeta struct {
	MachineID    string `json:"machine_id"`
	VsockUDSPath string `json:"vsock_uds_path"`
}

// writeSnapshotMeta records the machine ID and host vsock socket path beside
// snapshot.bin so a later restore cleans and dials the exact socket path.
func writeSnapshotMeta(snapDir, machineID, vsockPath string) error {
	raw, err := json.MarshalIndent(snapshotMeta{
		MachineID:    machineID,
		VsockUDSPath: vsockPath,
	}, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(filepath.Join(snapDir, snapshotMetaFile), raw, 0o644)
}

// resolveSnapshotVsockPath returns the host vsock UDS path stored in a
// snapshot. It reads meta.json first, then falls back to parsing snapshot.bin,
// and finally returns fallback.
func resolveSnapshotVsockPath(snapDir, snapPath, fallback string) string {
	if raw, err := os.ReadFile(filepath.Join(snapDir, snapshotMetaFile)); err == nil {
		var meta snapshotMeta
		if json.Unmarshal(raw, &meta) == nil && meta.VsockUDSPath != "" {
			return meta.VsockUDSPath
		}
	}
	if p := readSnapshotVsockPath(snapPath); p != "" {
		return p
	}
	return fallback
}

// readSnapshotVsockPath reads up to 4 MiB of snapshot.bin and extracts the
// serialized vsock UDS path if present.
func readSnapshotVsockPath(snapPath string) string {
	f, err := os.Open(snapPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 4<<20))
	if err != nil {
		return ""
	}
	return extractSnapshotVsockPath(data)
}

var vsockSuffix = []byte("/vsock.sock")

// extractSnapshotVsockPath locates a length-prefixed path ending in
// "/vsock.sock" inside a Firecracker snapshot state file. Firecracker uses
// unsigned LEB128 varint prefixes (postcard) in newer versions and 8-byte
// little-endian uint64 prefixes (bincode) in older versions.
func extractSnapshotVsockPath(data []byte) string {
	const maxPathLen = 4096
	searchFrom := 0
	for {
		rel := bytes.Index(data[searchFrom:], vsockSuffix)
		if rel < 0 {
			return ""
		}
		end := searchFrom + rel + len(vsockSuffix)
		low := end - maxPathLen
		if low < 1 {
			low = 1
		}
		// Scan from the longest possible start forward so we return the full
		// absolute path rather than an inner suffix.
		var vbuf [binary.MaxVarintLen64]byte
		for start := low; start <= end-len(vsockSuffix); start++ {
			if data[start] != '/' {
				continue
			}
			cand := data[start:end]
			if !isPrintablePath(cand) {
				continue
			}
			wantLen := uint64(len(cand))
			vn := binary.PutUvarint(vbuf[:], wantLen)
			if start >= vn && bytes.Equal(data[start-vn:start], vbuf[:vn]) {
				return string(cand)
			}
			if start >= 8 && binary.LittleEndian.Uint64(data[start-8:start]) == wantLen {
				return string(cand)
			}
		}
		searchFrom = end
	}
}

func isPrintablePath(b []byte) bool {
	if len(b) == 0 || b[0] != '/' {
		return false
	}
	for _, c := range b {
		if c <= 0x20 || c >= 0x7f {
			return false
		}
	}
	return true
}

// prepareVsockPath ensures the parent directory of vsockPath exists and removes
// a stale socket file left by an earlier run. It returns an error when a live
// process is actively listening on vsockPath.
func prepareVsockPath(vsockPath string) error {
	if vsockPath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(vsockPath), 0o755); err != nil {
		return fmt.Errorf("create vsock dir: %w", err)
	}
	if _, err := os.Lstat(vsockPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat vsock socket %q: %w", vsockPath, err)
	}
	conn, err := net.DialTimeout("unix", vsockPath, 200*time.Millisecond)
	if err == nil {
		conn.Close()
		return fmt.Errorf("vsock socket %q is already in use by a running VM", vsockPath)
	}
	if err := os.Remove(vsockPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale vsock socket %q: %w", vsockPath, err)
	}
	return nil
}
