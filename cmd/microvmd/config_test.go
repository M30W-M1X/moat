package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cfg.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfigFile(t *testing.T) {
	path := writeTemp(t, `{
	  "templates": [
	    {"name": "small", "vcpus": 1, "mem_mib": 256, "cpu_max": 0.5, "mem_max_mib": 512, "pids_max": 128},
	    {"name": "big", "kernel": "/k/vmlinux", "rootfs": "/r/rootfs.ext4", "vcpus": 4, "mem_mib": 2048}
	  ],
	  "tokens": [
	    {"id": "ci", "secret": "s3cr3t", "templates": ["small"], "max_concurrent": 4, "max_ttl": "20m", "deny_network": true},
	    {"id": "admin", "secret": "hunter2"}
	  ],
	  "defaults": {"global_max": 40, "pool": 2, "default_ttl": "10m"}
	}`)

	fc, err := loadConfigFile(path, "/default/vmlinux", "/default/rootfs.ext4")
	if err != nil {
		t.Fatalf("loadConfigFile: %v", err)
	}

	if len(fc.Templates) != 2 {
		t.Fatalf("templates = %d, want 2", len(fc.Templates))
	}
	small := fc.Templates[0]
	if small.Kernel != "/default/vmlinux" || small.RootFS != "/default/rootfs.ext4" {
		t.Errorf("small template did not inherit default paths: %+v", small)
	}
	if small.Limits.CPUCores != 0.5 || small.Limits.MemBytes != 512<<20 || small.Limits.PIDs != 128 {
		t.Errorf("small template limits wrong: %+v", small.Limits)
	}
	if fc.Templates[1].Kernel != "/k/vmlinux" || fc.Templates[1].VCPUs != 4 {
		t.Errorf("big template wrong: %+v", fc.Templates[1])
	}

	if fc.Tokens["ci"] != "s3cr3t" || fc.Tokens["admin"] != "hunter2" {
		t.Errorf("tokens wrong: %v", fc.Tokens)
	}
	if _, ok := fc.Policies["admin"]; ok {
		t.Error("admin has no restrictions, so it should have no policy entry")
	}
	p, ok := fc.Policies["ci"]
	if !ok {
		t.Fatal("ci should have a policy")
	}
	if len(p.Templates) != 1 || p.Templates[0] != "small" || p.MaxConcurrent != 4 ||
		p.MaxTTL != 20*time.Minute || !p.DenyNetwork {
		t.Errorf("ci policy wrong: %+v", p)
	}

	if fc.GlobalMax != 40 || fc.Pool != 2 || fc.DefaultTTL != 10*time.Minute {
		t.Errorf("defaults wrong: global=%d pool=%d ttl=%s", fc.GlobalMax, fc.Pool, fc.DefaultTTL)
	}
}

func TestLoadConfigFileErrors(t *testing.T) {
	cases := map[string]string{
		"no templates":         `{"tokens":[{"id":"a","secret":"b"}]}`,
		"no tokens":            `{"templates":[{"name":"t"}]}`,
		"token without secret": `{"templates":[{"name":"t"}],"tokens":[{"id":"a"}]}`,
		"unknown template ref": `{"templates":[{"name":"t"}],"tokens":[{"id":"a","secret":"b","templates":["nope"]}]}`,
		"bad duration":         `{"templates":[{"name":"t"}],"tokens":[{"id":"a","secret":"b","max_ttl":"20 minutes"}]}`,
		"unknown field":        `{"templates":[{"name":"t"}],"tokens":[{"id":"a","secret":"b"}],"bogus":1}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := loadConfigFile(writeTemp(t, body), "/k", "/r"); err == nil {
				t.Errorf("%s: expected an error", name)
			}
		})
	}
}
