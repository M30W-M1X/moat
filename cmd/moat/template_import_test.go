package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDeriveName(t *testing.T) {
	cases := map[string]string{
		"python:3.12":                          "python",
		"ghcr.io/org/app:v2":                   "app",
		"docker.io/library/python":             "python",
		"registry.io/team/svc@sha256:deadbeef": "svc",
		"NODE:20":                              "node",
	}
	for in, want := range cases {
		got, err := deriveName(in)
		if err != nil || got != want {
			t.Errorf("deriveName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := deriveName("some/really-long-image-name-that-is-way-too-long:1"); err == nil {
		t.Error("want an error for a name over 16 chars")
	}
}

func TestRegisterTemplateAppendAndDup(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "moatd.json")
	seed := `{"templates":[{"name":"default"}],"tokens":[{"id":"t","secret":"s"}]}`
	if err := os.WriteFile(p, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	e := templateEntry{Name: "python", RootFS: "/data/python-rootfs.ext4", Workdir: "/app"}
	if err := registerTemplate(p, e); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Templates []map[string]any `json:"templates"`
	}
	raw, _ := os.ReadFile(p)
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("result not valid JSON: %v", err)
	}
	if len(got.Templates) != 2 || got.Templates[1]["name"] != "python" {
		t.Fatalf("append wrong: %+v", got.Templates)
	}

	if err := registerTemplate(p, e); err == nil {
		t.Error("want a duplicate-name error on the second append")
	}
}

func TestWriteSidecar(t *testing.T) {
	dir := t.TempDir()
	path, err := writeSidecar(dir, templateEntry{Name: "python", RootFS: "/r"})
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, "python.template.json") {
		t.Fatalf("path = %q", path)
	}
	var e templateEntry
	raw, _ := os.ReadFile(path)
	if err := json.Unmarshal(raw, &e); err != nil || e.Name != "python" {
		t.Fatalf("sidecar bad: %v %+v", err, e)
	}
}

func TestTemplateImportDryRun(t *testing.T) {
	// --dry-run must not call podman. It prints the plan and returns 0.
	code := templateImport([]string{"--dry-run", "python:3.12"})
	if code != 0 {
		t.Fatalf("dry-run exit = %d, want 0", code)
	}
}
