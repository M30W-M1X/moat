package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSidecars(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, e templateEntry) {
		b, _ := json.Marshal(e)
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("python.template.json", templateEntry{Name: "python", RootFS: "/r/python", Workdir: "/app"})
	write("node.template.json", templateEntry{Name: "node", RootFS: "/r/node"})
	write("broken.template.json", templateEntry{}) // Name empty -> skipped
	if err := os.WriteFile(filepath.Join(dir, "notjson.template.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := loadSidecars(dir)
	if len(got) != 2 {
		t.Fatalf("loadSidecars returned %d entries, want 2: %+v", len(got), got)
	}
}

func TestTemplateShowFromSidecar(t *testing.T) {
	dir := t.TempDir()
	b, _ := json.Marshal(templateEntry{Name: "python", RootFS: "/r/python", Workdir: "/app", DefaultArgv: []string{"python3"}})
	if err := os.WriteFile(filepath.Join(dir, "python.template.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if code := templateShow([]string{"--data", dir, "python"}); code != 0 {
		t.Fatalf("show exit = %d, want 0", code)
	}
	if code := templateShow([]string{"--data", dir, "missing"}); code != 1 {
		t.Fatalf("show missing exit = %d, want 1", code)
	}
}
