//go:build integration

package imgimport_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/M30W-M1X/moat/internal/imgimport"
)

// buildTinyImage builds a throwaway image with a known config and returns its
// local tag.
func buildTinyImage(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("no podman")
	}
	dir := t.TempDir()
	cf := `FROM docker.io/library/alpine@sha256:c64c687cbea9300178b30c95835354e34c4e4febc4badfe27102879de0483b5e
ENV MOAT_TEST_ENV=from-image
WORKDIR /srv
ENTRYPOINT ["/bin/echo"]
CMD ["hello"]
`
	if err := os.WriteFile(filepath.Join(dir, "Containerfile"), []byte(cf), 0o644); err != nil {
		t.Fatal(err)
	}
	tag := "moat-imgimport-test:latest"
	if out, err := exec.Command("podman", "build", "-q", "-t", tag, "-f", filepath.Join(dir, "Containerfile"), dir).CombinedOutput(); err != nil {
		t.Fatalf("podman build: %v: %s", err, out)
	}
	t.Cleanup(func() { exec.Command("podman", "rmi", "-f", tag).Run() })
	return tag
}

func agentBinary(t *testing.T) string {
	t.Helper()
	// The test runs from internal/imgimport; the repo root is two levels up.
	p, err := filepath.Abs(filepath.Join("..", "..", "bin", "guest-agent"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Skipf("guest agent missing (run: make agent): %v", err)
	}
	return p
}

func TestImportBuildsRootfsAndReadsConfig(t *testing.T) {
	tag := buildTinyImage(t)
	agent := agentBinary(t)
	out := t.TempDir()

	res, err := imgimport.Import(context.Background(), imgimport.Source{Ref: tag}, imgimport.Options{
		Name: "tiny", AgentPath: agent, OutDir: out,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Env["MOAT_TEST_ENV"] != "from-image" {
		t.Errorf("Env = %v, want MOAT_TEST_ENV=from-image", res.Env)
	}
	if res.Dir != "/srv" {
		t.Errorf("Dir = %q, want /srv", res.Dir)
	}
	if want := []string{"/bin/echo", "hello"}; strings.Join(res.DefaultArgv, " ") != strings.Join(want, " ") {
		t.Errorf("DefaultArgv = %v, want %v", res.DefaultArgv, want)
	}
	fi, err := os.Stat(res.RootFSPath)
	if err != nil || fi.Size() < 1<<20 {
		t.Fatalf("rootfs stat: %v size=%v", err, fi)
	}
	// The agent must be inside the image.
	dbg, err := exec.Command("debugfs", "-R", "stat /sbin/guest-agent", res.RootFSPath).CombinedOutput()
	if err != nil || strings.Contains(string(dbg), "File not found") {
		t.Fatalf("guest-agent not in rootfs: %v: %s", err, dbg)
	}
}

func TestImportFromArchive(t *testing.T) {
	tag := buildTinyImage(t)
	agent := agentBinary(t)
	arc := filepath.Join(t.TempDir(), "img.tar")
	if out, err := exec.Command("podman", "save", "-o", arc, tag).CombinedOutput(); err != nil {
		t.Fatalf("podman save: %v: %s", err, out)
	}
	res, err := imgimport.Import(context.Background(), imgimport.Source{Archive: arc}, imgimport.Options{
		Name: "tiny-arc", AgentPath: agent, OutDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Dir != "/srv" {
		t.Errorf("Dir = %q, want /srv", res.Dir)
	}
}
