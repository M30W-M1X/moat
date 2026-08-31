package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/M30W-M1X/moat/internal/cgroup"
	"github.com/M30W-M1X/moat/internal/server"
)

// fileConfig is the on-disk shape of the -config file. It defines the templates,
// the auth tokens, the per-token policy, and optional server defaults. Flags
// still set anything the file leaves at its zero value.
type fileConfig struct {
	Templates      []fileTemplate `json:"templates"`
	Tokens         []fileToken    `json:"tokens"`
	Defaults       fileDefaults   `json:"defaults"`
	Audit          *fileAudit     `json:"audit"`
	ImageVerify    string         `json:"image_verify"`
	CosignIdentity string         `json:"cosign_identity"`
	CosignIssuer   string         `json:"cosign_issuer"`
}

// fileAudit is the on-disk shape of the optional "audit" block. An empty "dir"
// disables the audit log. A missing block keeps the flag or the built-in value.
type fileAudit struct {
	Dir           string `json:"dir"`
	MaxFileMB     int    `json:"max_file_mb"`
	MaxTotalMB    int    `json:"max_total_mb"`
	MaxAge        string `json:"max_age"`
	FsyncBatch    int    `json:"fsync_batch"`
	FsyncInterval string `json:"fsync_interval"`
}

type fileTemplate struct {
	Name      string  `json:"name"`
	Kernel    string  `json:"kernel"`      // empty = the built-in default vmlinux
	RootFS    string  `json:"rootfs"`      // empty = the built-in default rootfs.ext4
	VCPUs     int64   `json:"vcpus"`       // default 1
	MemMiB    int64   `json:"mem_mib"`     // default 256
	CPUMax    float64 `json:"cpu_max"`     // cgroup CPU cores; 0 = no cap
	MemMaxMiB int64   `json:"mem_max_mib"` // cgroup memory.max; 0 = no cap
	PidsMax   int64   `json:"pids_max"`    // cgroup pids.max; 0 = no cap
	DiskBW    int64   `json:"disk_bw"`     // rootfs bytes/sec; 0 = no limit
	NetBW     int64   `json:"net_bw"`      // network bytes/sec; 0 = no limit
}

type fileToken struct {
	ID            string   `json:"id"`
	Secret        string   `json:"secret"`
	Templates     []string `json:"templates"`      // allowed template names; empty = all
	MaxConcurrent int      `json:"max_concurrent"` // 0 = server token_max
	MaxTTL        string   `json:"max_ttl"`        // duration string; empty = server max_ttl
	DenyNetwork   bool     `json:"deny_network"`
	AllowSecrets  bool     `json:"allow_secret_override"` // may pass secret-flagged content
	AuditRead     string   `json:"audit_read"`            // "all" = may read the audit log
}

type fileDefaults struct {
	GlobalMax  int    `json:"global_max"`
	TokenMax   int    `json:"token_max"`
	DefaultTTL string `json:"default_ttl"`
	MaxTTL     string `json:"max_ttl"`
	Pool       int    `json:"pool"`
	NetPool    string `json:"net_pool"`
	Uplink     string `json:"uplink"`
}

// loadedConfig is the result of reading a -config file. Zero-valued fields mean
// "the file did not set this; keep the flag value".
type loadedConfig struct {
	Templates    []server.Template
	Tokens       map[string]string
	Policies     map[string]server.TokenPolicy
	GlobalMax    int
	TokenMax     int
	DefaultTTL   time.Duration
	MaxTTL       time.Duration
	Pool         int
	NetPool      string
	Uplink       string
	Audit        *loadedAudit
	ConfigSHA256 string // hex sha256 of the raw file

	ImageVerify    string
	CosignIdentity string
	CosignIssuer   string
}

// loadedAudit is the parsed "audit" block. Set reports that the file had the
// block. Durations come from the string form.
type loadedAudit struct {
	Dir           string
	MaxFileMB     int
	MaxTotalMB    int
	MaxAge        time.Duration
	FsyncBatch    int
	FsyncInterval time.Duration
	Set           bool
}

// loadConfigFile reads and validates the file at path. dm supplies the default
// kernel and rootfs paths for templates that omit them.
func loadConfigFile(path string, defKernel, defRootFS string) (loadedConfig, error) {
	var out loadedConfig
	raw, err := os.ReadFile(path)
	if err != nil {
		return out, err
	}
	sum := sha256.Sum256(raw)
	out.ConfigSHA256 = hex.EncodeToString(sum[:])
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var fc fileConfig
	if err := dec.Decode(&fc); err != nil {
		return out, fmt.Errorf("parse %s: %w", path, err)
	}

	if len(fc.Templates) == 0 {
		return out, fmt.Errorf("%s: at least one template is required", path)
	}
	if len(fc.Tokens) == 0 {
		return out, fmt.Errorf("%s: at least one token is required", path)
	}

	known := map[string]bool{}
	for _, ft := range fc.Templates {
		if ft.Name == "" {
			return out, fmt.Errorf("%s: a template has no name", path)
		}
		if known[ft.Name] {
			return out, fmt.Errorf("%s: duplicate template %q", path, ft.Name)
		}
		known[ft.Name] = true
		t := server.Template{
			Name:   ft.Name,
			Kernel: orDefault(ft.Kernel, defKernel),
			RootFS: orDefault(ft.RootFS, defRootFS),
			VCPUs:  orInt64(ft.VCPUs, 1),
			MemMiB: orInt64(ft.MemMiB, 256),
			Limits: cgroup.Limits{
				CPUCores: ft.CPUMax,
				MemBytes: ft.MemMaxMiB << 20,
				PIDs:     ft.PidsMax,
			},
			DiskBW: ft.DiskBW,
			NetBW:  ft.NetBW,
		}
		out.Templates = append(out.Templates, t)
	}

	out.Tokens = map[string]string{}
	out.Policies = map[string]server.TokenPolicy{}
	for _, ft := range fc.Tokens {
		if ft.ID == "" || ft.Secret == "" {
			return out, fmt.Errorf("%s: a token is missing id or secret", path)
		}
		if _, dup := out.Tokens[ft.ID]; dup {
			return out, fmt.Errorf("%s: duplicate token id %q", path, ft.ID)
		}
		out.Tokens[ft.ID] = ft.Secret

		var maxTTL time.Duration
		if ft.MaxTTL != "" {
			maxTTL, err = time.ParseDuration(ft.MaxTTL)
			if err != nil {
				return out, fmt.Errorf("%s: token %q max_ttl: %w", path, ft.ID, err)
			}
		}
		for _, name := range ft.Templates {
			if !known[name] {
				return out, fmt.Errorf("%s: token %q allows unknown template %q", path, ft.ID, name)
			}
		}
		if len(ft.Templates) > 0 || ft.MaxConcurrent > 0 || maxTTL > 0 || ft.DenyNetwork || ft.AllowSecrets || ft.AuditRead != "" {
			if ft.AuditRead != "" && ft.AuditRead != "all" {
				return out, fmt.Errorf("%s: token %q audit_read must be \"all\" or empty", path, ft.ID)
			}
			out.Policies[ft.ID] = server.TokenPolicy{
				Templates:           ft.Templates,
				MaxConcurrent:       ft.MaxConcurrent,
				MaxTTL:              maxTTL,
				DenyNetwork:         ft.DenyNetwork,
				AllowSecretOverride: ft.AllowSecrets,
				AuditRead:           ft.AuditRead,
			}
		}
	}

	switch fc.ImageVerify {
	case "", "enforce", "warn", "off":
		out.ImageVerify = fc.ImageVerify
	default:
		return out, fmt.Errorf("%s: image_verify must be enforce, warn, or off", path)
	}
	out.CosignIdentity = fc.CosignIdentity
	out.CosignIssuer = fc.CosignIssuer

	d := fc.Defaults
	out.GlobalMax, out.TokenMax, out.Pool = d.GlobalMax, d.TokenMax, d.Pool
	out.NetPool, out.Uplink = d.NetPool, d.Uplink
	if d.DefaultTTL != "" {
		if out.DefaultTTL, err = time.ParseDuration(d.DefaultTTL); err != nil {
			return out, fmt.Errorf("%s: defaults.default_ttl: %w", path, err)
		}
	}
	if d.MaxTTL != "" {
		if out.MaxTTL, err = time.ParseDuration(d.MaxTTL); err != nil {
			return out, fmt.Errorf("%s: defaults.max_ttl: %w", path, err)
		}
	}

	if fc.Audit != nil {
		la := &loadedAudit{
			Dir:        fc.Audit.Dir,
			MaxFileMB:  fc.Audit.MaxFileMB,
			MaxTotalMB: fc.Audit.MaxTotalMB,
			FsyncBatch: fc.Audit.FsyncBatch,
			Set:        true,
		}
		if fc.Audit.MaxAge != "" {
			if la.MaxAge, err = time.ParseDuration(fc.Audit.MaxAge); err != nil {
				return out, fmt.Errorf("%s: audit.max_age: %w", path, err)
			}
		}
		if fc.Audit.FsyncInterval != "" {
			if la.FsyncInterval, err = time.ParseDuration(fc.Audit.FsyncInterval); err != nil {
				return out, fmt.Errorf("%s: audit.fsync_interval: %w", path, err)
			}
		}
		out.Audit = la
	}
	return out, nil
}

func orInt64(v, def int64) int64 {
	if v > 0 {
		return v
	}
	return def
}
