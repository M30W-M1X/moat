package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupFakeInstall(t *testing.T) (prefix, dataDir, cfgDir, unitPath string) {
	t.Helper()
	root := t.TempDir()
	prefix = filepath.Join(root, "bin")
	dataDir = filepath.Join(root, "data")
	cfgDir = filepath.Join(root, "config")
	unitDir := filepath.Join(root, "systemd")
	for _, d := range []string{prefix, dataDir, cfgDir, unitDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, bin := range installedBinaries {
		if err := os.WriteFile(filepath.Join(prefix, bin), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(prefix, "unrelated"), []byte("keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "rootfs.ext4"), []byte("img"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "moatd.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	unitPath = filepath.Join(unitDir, "moatd.service")
	if err := os.WriteFile(unitPath, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return prefix, dataDir, cfgDir, unitPath
}

func TestExecuteUninstallFull(t *testing.T) {
	prefix, dataDir, cfgDir, unitPath := setupFakeInstall(t)
	var cmds []string
	var buf bytes.Buffer

	err := executeUninstall(uninstallOptions{
		UserMode:  true,
		Prefix:    prefix,
		DataDir:   dataDir,
		ConfigDir: cfgDir,
		UnitPath:  unitPath,
		Stdout:    &buf,
		RunCmd: func(name string, args ...string) error {
			cmds = append(cmds, name+" "+strings.Join(args, " "))
			return nil
		},
	})
	if err != nil {
		t.Fatalf("executeUninstall: %v", err)
	}

	for _, bin := range installedBinaries {
		if _, err := os.Stat(filepath.Join(prefix, bin)); !os.IsNotExist(err) {
			t.Errorf("binary %s still exists", bin)
		}
	}
	if _, err := os.Stat(filepath.Join(prefix, "unrelated")); err != nil {
		t.Errorf("unrelated binary was removed: %v", err)
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Errorf("data dir still exists")
	}
	if _, err := os.Stat(cfgDir); !os.IsNotExist(err) {
		t.Errorf("config dir still exists")
	}
	if _, err := os.Stat(unitPath); !os.IsNotExist(err) {
		t.Errorf("unit file still exists")
	}
	if len(cmds) != 2 || !strings.Contains(cmds[0], "disable --now moatd.service") {
		t.Errorf("unexpected systemctl calls: %v", cmds)
	}
}

func TestExecuteUninstallDryRunAndKeepFlags(t *testing.T) {
	prefix, dataDir, cfgDir, unitPath := setupFakeInstall(t)
	var buf bytes.Buffer

	// Dry run must leave every file intact.
	if err := executeUninstall(uninstallOptions{
		Prefix:    prefix,
		DataDir:   dataDir,
		ConfigDir: cfgDir,
		UnitPath:  unitPath,
		DryRun:    true,
		Stdout:    &buf,
		RunCmd: func(name string, args ...string) error {
			t.Errorf("dry-run invoked command %s %v", name, args)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(prefix, "moat")); err != nil {
		t.Error("dry-run removed moat binary")
	}

	// KeepData and KeepConfig preserve data and config directories.
	if err := executeUninstall(uninstallOptions{
		Prefix:     prefix,
		DataDir:    dataDir,
		ConfigDir:  cfgDir,
		UnitPath:   unitPath,
		KeepData:   true,
		KeepConfig: true,
		Stdout:     &buf,
		RunCmd:     func(string, ...string) error { return nil },
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(prefix, "moat")); !os.IsNotExist(err) {
		t.Error("moat binary was not removed")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "rootfs.ext4")); err != nil {
		t.Error("KeepData did not preserve data dir")
	}
	if _, err := os.Stat(filepath.Join(cfgDir, "moatd.json")); err != nil {
		t.Error("KeepConfig did not preserve config dir")
	}
}
