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

func TestLoadConfigAudit(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "moatd.json")
	os.WriteFile(p, []byte(`{
	  "templates":[{"name":"default"}],
	  "tokens":[{"id":"ci","secret":"s"},{"id":"secops","secret":"s2","audit_read":"all"}],
	  "audit":{"dir":"`+dir+`/aud","max_file_mb":64,"max_age":"48h"}
	}`), 0o600)
	lc, err := loadConfigFile(p, "/k", "/r")
	if err != nil {
		t.Fatal(err)
	}
	if lc.Policies["secops"].AuditRead != "all" {
		t.Fatalf("audit_read not parsed: %+v", lc.Policies["secops"])
	}
	if lc.Audit == nil || lc.Audit.Dir != dir+"/aud" || lc.Audit.MaxFileMB != 64 {
		t.Fatalf("audit block not parsed: %+v", lc.Audit)
	}
	if lc.Audit.MaxAge != 48*time.Hour {
		t.Fatalf("max_age = %v", lc.Audit.MaxAge)
	}
}

func TestConfigImageVerifyValidation(t *testing.T) {
	base := `{"templates":[{"name":"default"}],"tokens":[{"id":"a","secret":"b"}]`
	good := writeTemp(t, base+`,"image_verify":"warn"}`)
	if lc, err := loadConfigFile(good, "k", "r"); err != nil {
		t.Fatalf("image_verify=warn should load: %v", err)
	} else if lc.ImageVerify != "warn" {
		t.Fatalf("ImageVerify = %q, want warn", lc.ImageVerify)
	}
	bad := writeTemp(t, base+`,"image_verify":"loud"}`)
	if _, err := loadConfigFile(bad, "k", "r"); err == nil {
		t.Fatal("image_verify=loud should be rejected")
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

func TestLoadConfigTemplateExecDefaults(t *testing.T) {
	path := writeTemp(t, `{
	  "templates": [
	    { "name": "python", "rootfs": "/data/python-rootfs.ext4",
	      "env": {"PATH": "/usr/local/bin:/usr/bin", "PYTHONUNBUFFERED": "1"},
	      "workdir": "/app",
	      "default_argv": ["python3"],
	      "source": "docker.io/library/python@sha256:abc" }
	  ],
	  "tokens": [ { "id": "t", "secret": "s" } ]
	}`)
	cfg, err := loadConfigFile(path, "/data/vmlinux", "/data/rootfs.ext4")
	if err != nil {
		t.Fatal(err)
	}
	tpl := cfg.Templates[0]
	if tpl.Dir != "/app" {
		t.Errorf("Dir = %q, want /app", tpl.Dir)
	}
	if tpl.Env["PYTHONUNBUFFERED"] != "1" || tpl.Env["PATH"] == "" {
		t.Errorf("Env = %v", tpl.Env)
	}
	if len(tpl.DefaultArgv) != 1 || tpl.DefaultArgv[0] != "python3" {
		t.Errorf("DefaultArgv = %v", tpl.DefaultArgv)
	}
	if tpl.Source == "" {
		t.Errorf("Source not carried")
	}
}

func TestLoadConfigTemplateWithoutExecDefaults(t *testing.T) {
	path := writeTemp(t, `{"templates":[{"name":"default"}],"tokens":[{"id":"t","secret":"s"}]}`)
	cfg, err := loadConfigFile(path, "/k", "/r")
	if err != nil {
		t.Fatal(err)
	}
	tpl := cfg.Templates[0]
	if tpl.Env != nil || tpl.Dir != "" || tpl.DefaultArgv != nil {
		t.Errorf("unset exec defaults must stay zero: %+v", tpl)
	}
}

func TestLoadConfigProductionFields(t *testing.T) {
	path := writeTemp(t, `{
	  "templates": [
	    {
	      "name": "fast",
	      "pool": 3,
	      "snapshot_dir": "/var/lib/moat/snaps/fast",
	      "use_uffd": true,
	      "read_only_root": true,
	      "scratch_mib": 1024
	    }
	  ],
	  "tokens": [
	    {
	      "id": "ci",
	      "secret": "s3cr3t",
	      "egress_allow": ["pypi.org"],
	      "egress_deny": ["evil.pypi.org"]
	    }
	  ],
	  "defaults": {
	    "jail": true,
	    "jail_uid": 1001,
	    "jail_gid": 1001,
	    "egress_allow": ["github.com"],
	    "egress_deny": ["blocked.example"],
	    "strict_egress": true
	  },
	  "socket": "/run/moat/moatd.sock",
	  "tls_cert": "/etc/moat/server.pem",
	  "tls_key": "/etc/moat/server.key",
	  "tls_client_ca": "/etc/moat/ca.pem",
	  "audit": {
	    "dir": "/var/lib/moat/audit",
	    "webhook_url": "https://siem.example/events"
	  }
	}`)

	lc, err := loadConfigFile(path, "/k", "/r")
	if err != nil {
		t.Fatalf("loadConfigFile: %v", err)
	}
	tpl := lc.Templates[0]
	if tpl.PoolSize != 3 || tpl.SnapshotDir != "/var/lib/moat/snaps/fast" || !tpl.UseUFFD || !tpl.ReadOnlyRoot || tpl.ScratchMiB != 1024 {
		t.Fatalf("template production fields wrong: %+v", tpl)
	}
	pol := lc.Policies["ci"]
	if len(pol.EgressAllow) != 1 || pol.EgressAllow[0] != "pypi.org" || len(pol.EgressDeny) != 1 || pol.EgressDeny[0] != "evil.pypi.org" {
		t.Fatalf("token egress policy wrong: %+v", pol)
	}
	if !lc.Jail || lc.JailUID != 1001 || lc.JailGID != 1001 || !lc.StrictEgress {
		t.Fatalf("defaults hardening fields wrong: %+v", lc)
	}
	if len(lc.EgressAllow) != 1 || lc.EgressAllow[0] != "github.com" || len(lc.EgressDeny) != 1 || lc.EgressDeny[0] != "blocked.example" {
		t.Fatalf("default egress lists wrong: allow=%v deny=%v", lc.EgressAllow, lc.EgressDeny)
	}
	if lc.Socket != "/run/moat/moatd.sock" || lc.TLSCert != "/etc/moat/server.pem" || lc.TLSKey != "/etc/moat/server.key" || lc.TLSClientCA != "/etc/moat/ca.pem" {
		t.Fatalf("transport fields wrong: socket=%q cert=%q key=%q ca=%q", lc.Socket, lc.TLSCert, lc.TLSKey, lc.TLSClientCA)
	}
	if lc.Audit == nil || lc.Audit.WebhookURL != "https://siem.example/events" {
		t.Fatalf("audit webhook wrong: %+v", lc.Audit)
	}
}

func TestLoadConfigTLSValidation(t *testing.T) {
	base := `{"templates":[{"name":"t"}],"tokens":[{"id":"a","secret":"b"}]`
	cases := map[string]string{
		"cert without key":      base + `,"tls_cert":"/a.pem"}`,
		"key without cert":      base + `,"tls_key":"/a.key"}`,
		"client ca without tls": base + `,"tls_client_ca":"/ca.pem"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := loadConfigFile(writeTemp(t, body), "/k", "/r"); err == nil {
				t.Errorf("%s: expected validation error", name)
			}
		})
	}
}

func TestBuildTLSConfigValidation(t *testing.T) {
	if cfg, err := buildTLSConfig("", "", ""); err != nil || cfg != nil {
		t.Fatalf("empty tls = (%v, %v), want (nil, nil)", cfg, err)
	}
	if _, err := buildTLSConfig("/cert", "", ""); err == nil {
		t.Error("expected error for cert without key")
	}
	if _, err := buildTLSConfig("", "", "/ca"); err == nil {
		t.Error("expected error for client CA without cert/key")
	}
	cfg, err := buildTLSConfig("/cert", "/key", "")
	if err != nil || cfg == nil || cfg.MinVersion == 0 {
		t.Fatalf("tls without client CA = (%+v, %v)", cfg, err)
	}
}

func TestExampleConfigLoads(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "moatd.example.json")
	lc, err := loadConfigFile(path, "/k", "/r")
	if err != nil {
		t.Fatalf("loadConfigFile(%s): %v", path, err)
	}
	if len(lc.Templates) == 0 || len(lc.Tokens) == 0 {
		t.Fatalf("example config missing templates or tokens")
	}
}
