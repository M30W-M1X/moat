package main

import (
	"os"
	"testing"
)

func TestParseAPKInstalled(t *testing.T) {
	raw, err := os.ReadFile("testdata/apk-installed.txt")
	if err != nil {
		t.Fatal(err)
	}
	pkgs := parseAPKInstalled(raw)
	if len(pkgs) != 2 {
		t.Fatalf("got %d packages, want 2", len(pkgs))
	}
	if pkgs[0].Name != "musl" || pkgs[0].Version != "1.2.5-r0" ||
		pkgs[0].Arch != "x86_64" || pkgs[0].License != "MIT" {
		t.Fatalf("package 0 wrong: %+v", pkgs[0])
	}
	if pkgs[1].Name != "busybox" || pkgs[1].License != "GPL-2.0-only" {
		t.Fatalf("package 1 wrong: %+v", pkgs[1])
	}
}

func TestParseGoModules(t *testing.T) {
	raw, err := os.ReadFile("testdata/go-version-m.txt")
	if err != nil {
		t.Fatal(err)
	}
	mods := parseGoModules(raw)
	if len(mods) != 2 {
		t.Fatalf("got %d modules, want 2 (deps only)", len(mods))
	}
	if mods[0].Path != "google.golang.org/grpc" || mods[0].Version != "v1.71.0" {
		t.Fatalf("module 0 wrong: %+v", mods[0])
	}
}
