package vm

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestCopyRootFS(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.ext4")
	dst := filepath.Join(dir, "dst.ext4")

	// Create a file with a sparse hole in the middle.
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("head")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(1<<20, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("tail")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if err := CopyRootFS(src, dst); err != nil {
		t.Fatalf("CopyRootFS: %v", err)
	}
	want, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("copied bytes differ: got %d bytes, want %d", len(got), len(want))
	}
}
