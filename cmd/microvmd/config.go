package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/M30W-M1X/microvm_controller/internal/cgroup"
	"github.com/M30W-M1X/microvm_controller/internal/server"
)

// fileConfig is the on-disk shape of the -config file. It defines the templates,
// the auth tokens, the per-token policy, and optional server defaults. Flags
// still set anything the file leaves at its zero value.
type fileConfig struct {
	Templates []fileTemplate `json:"templates"`
	Tokens    []fileToken    `json:"tokens"`
	Defaults  fileDefaults   `json:"defaults"`
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
	Templates  []server.Template
	Tokens     map[string]string
	Policies   map[string]server.TokenPolicy
	GlobalMax  int
	TokenMax   int
	DefaultTTL time.Duration
	MaxTTL     time.Duration
	Pool       int
	NetPool    string
	Uplink     string
}

// loadConfigFile reads and validates the file at path. dm supplies the default
// kernel and rootfs paths for templates that omit them.
func loadConfigFile(path string, defKernel, defRootFS string) (loadedConfig, error) {
	var out loadedConfig
	raw, err := os.ReadFile(path)
	if err != nil {
		return out, err
	}
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
		if len(ft.Templates) > 0 || ft.MaxConcurrent > 0 || maxTTL > 0 || ft.DenyNetwork {
			out.Policies[ft.ID] = server.TokenPolicy{
				Templates:     ft.Templates,
				MaxConcurrent: ft.MaxConcurrent,
				MaxTTL:        maxTTL,
				DenyNetwork:   ft.DenyNetwork,
			}
		}
	}

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
	return out, nil
}

func orInt64(v, def int64) int64 {
	if v > 0 {
		return v
	}
	return def
}
