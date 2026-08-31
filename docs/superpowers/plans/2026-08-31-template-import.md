# `moat template import` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `moat template import` — one command that turns an OCI container image (registry pull or local archive) into a Moat rootfs template, carrying the image's `ENV` / `WORKDIR` / `ENTRYPOINT` forward as exec defaults.

**Architecture:** A new `internal/imgimport` package runs the pipeline (podman pull/load → pin digest → parse image config → `podman export` → inject the guest agent → auto-size → `mkfs.ext4 -d`). `cmd/moat` gains `template import` and `template show`. `server.Template` and the `moatd.json` `templates` entry gain four optional fields (`env`, `workdir`, `default_argv`, `source`). `handleExec` folds the template defaults under the request through a pure `mergeExec` helper.

**Tech Stack:** Go 1.27, `podman` and `mkfs.ext4` as subprocesses (already required deps), the standard library only. No new Go module.

**Spec:** `docs/superpowers/specs/2026-08-31-template-import-design.md`

## Global Constraints

- **Go 1.27.0 exactly.** `toolchain go1.27.0` in `go.mod`; the Makefile exports `GOTOOLCHAIN=local`.
- **No new Go module dependency.** Shell out to `podman` / `mkfs.ext4`; parse output with `encoding/json` and `strings`.
- **ASD-STE100 for every comment and every log / error string.** Active voice. One instruction per sentence. ≤ 20 words for instructions, ≤ 25 for descriptions. No semicolons. No phrasal verbs ("start" not "spin up", "remove" not "take off"). Simple tenses. Use "start" / "stop" for VM lifecycle.
- **Branch off `main`.** Work on `template-import` (already created at `f76088e`; spec committed `b5f0e9d`). Never merge to `main` without explicit user approval.
- **Rootless.** `podman export | tar -x` and `mkfs.ext4 -d`. No loop mount. No root (Gotcha `G-4`).
- **Determinism knobs match `build/build-rootfs.sh`:** `SOURCE_DATE_EPOCH` default `1788134400`, eager inode / journal init, a filesystem UUID and a hash seed derived from the template name so a re-import of the same name and digest is byte-stable.
- **Not written to `images.lock`.** An imported template records the image digest in `source`.
- **Never pipe a producer into `head -c` under `set -o pipefail`** (SIGPIPE, exit 141 — Gotcha `G-18` family). Not expected here; the pipeline is Go.
- **Audit events stay metadata only** — never a payload, env value, file body, or command output. `env_keys` records key names, never values.

---

## File Structure

**Create:**
- `internal/imgimport/imgimport.go` — `Source`, `Options`, `Result` types; `Import()` orchestration; temp-dir lifecycle; `.tmp` rename.
- `internal/imgimport/podman.go` — podman subprocess calls (`pull`, `load`, `inspect`, `create`, `export`, `rm`); `inspectImage` struct; `parseImageConfig`.
- `internal/imgimport/rootfs.go` — filesystem assembly: extract, inject agent, scaffolding, `/bin/sh` check, timestamp normalise, auto-size, `mkfs.ext4`; `uuidFromName` / `seedFromName`.
- `internal/imgimport/imgimport_test.go` — unit tests (no podman): `parseImageConfig`, `deriveName` is in cmd/moat, `uuidFromName`, `seedFromName`, `autoSizeBytes`.
- `internal/imgimport/imgimport_integration_test.go` — `//go:build integration`: build a tiny image in-test, `Import` it, assert `Result` + rootfs contents.
- `cmd/moat/template_import.go` — `templateImport(args []string) int`; `deriveName`; `registerTemplate`; the JSON entry type shared with the sidecar.
- `cmd/moat/template_import_test.go` — unit tests: `deriveName`, `--dry-run` output, `registerTemplate` append + dup rejection + round-trip through `loadConfigFile` (cross-package via `os/exec` of a built binary is overkill — call `loadConfigFile`? it is in package `main` of `cmd/moatd`. Instead validate the written JSON parses and has the entry).
- `cmd/moat/template_show.go` — `templateShow(args []string) int`.
- `cmd/moat/template_show_test.go` — unit tests with temp sidecar files.
- `internal/server/exec_merge.go` — `mergeExec` pure helper + `errNoArgv`.
- `internal/server/exec_merge_test.go` — unit tests for `mergeExec`.

**Modify:**
- `cmd/moat/template.go` — `runTemplate` dispatches `import` and `show`; expand help text; `templateList` scans `<data>/*.template.json`.
- `cmd/moatd/config.go` — `fileTemplate` gains `Env` / `Workdir` / `DefaultArgv` / `Source`; the `server.Template{...}` literal copies them.
- `cmd/moatd/config_test.go` — a config with the new fields loads into `server.Template`; a config without them is unchanged.
- `internal/server/sandbox.go` — `Template` gains `Env` / `Dir` / `DefaultArgv` / `Source`; `Sandbox` gains an unexported `tpl Template`.
- `internal/server/server.go` — `handleCreate` sets `sb.tpl = tpl`; `handleExec` calls `mergeExec`, passes the merged `argv` / `env` / `dir` to `Exec`, injects the proxy vars into the merged `env`, and records `argv_source` plus the union `env_keys`.
- `internal/server/server_integration_test.go` — `TestExecTemplateDefaults`.
- `docs/moatd.example.json` — one imported-style template entry.
- `build/templates/README.md`, `docs/developer-guide.md`, `docs/developer-guide.html`, `README.md` — describe `moat template import`.

---

## Task 1: `internal/imgimport` — podman layer and image-config parsing

**Files:**
- Create: `internal/imgimport/podman.go`
- Create: `internal/imgimport/imgimport_test.go`

**Interfaces:**
- Produces:
  - `type inspectImage struct { ID string; RepoDigests []string; Architecture string; Os string; Config struct { Env, Cmd, Entrypoint []string; WorkingDir string } }`
  - `func podmanInspect(ctx context.Context, ref string) (inspectImage, error)` — runs `podman inspect --type image <ref>`, unmarshals `[]inspectImage`, returns `[0]`.
  - `func parseImageConfig(im inspectImage) (env map[string]string, dir string, defaultArgv []string, warnings []string)`.
  - `func podmanPull(ctx context.Context, ref string) error` — `podman pull --arch amd64 --os linux <ref>`.
  - `func podmanLoad(ctx context.Context, archive string) (ref string, err error)` — `podman load -i <archive>`, parses `Loaded image: <ref>` from stdout.
  - `func podmanExportRootfs(ctx context.Context, ref, destDir string) error` — `cid := podman create <ref> /bin/true`; `podman export <cid> | tar -x -C destDir`; `podman rm -f <cid>` in a defer.
  - `func checkPodman() error` — `exec.LookPath("podman")`; error `"podman is required for template import"`.

- [ ] **Step 1: Write the failing test for `parseImageConfig`**

`internal/imgimport/imgimport_test.go`:

```go
package imgimport

import (
	"reflect"
	"testing"
)

func TestParseImageConfig(t *testing.T) {
	im := inspectImage{}
	im.Config.Env = []string{"PATH=/usr/local/bin:/usr/bin", "LANG=C.UTF-8", "EMPTY="}
	im.Config.WorkingDir = "/app"
	im.Config.Entrypoint = []string{"/entrypoint.sh"}
	im.Config.Cmd = []string{"python3", "app.py"}

	env, dir, argv, warns := parseImageConfig(im)

	wantEnv := map[string]string{"PATH": "/usr/local/bin:/usr/bin", "LANG": "C.UTF-8", "EMPTY": ""}
	if !reflect.DeepEqual(env, wantEnv) {
		t.Errorf("env = %v, want %v", env, wantEnv)
	}
	if dir != "/app" {
		t.Errorf("dir = %q, want /app", dir)
	}
	if want := []string{"/entrypoint.sh", "python3", "app.py"}; !reflect.DeepEqual(argv, want) {
		t.Errorf("argv = %v, want %v", argv, want)
	}
	if len(warns) != 0 {
		t.Errorf("warnings = %v, want none", warns)
	}
}

func TestParseImageConfigNoDefaultCommand(t *testing.T) {
	_, dir, argv, warns := parseImageConfig(inspectImage{})
	if dir != "" || argv != nil {
		t.Errorf("dir = %q argv = %v, want empty", dir, argv)
	}
	if len(warns) != 1 {
		t.Fatalf("warnings = %v, want one about no default command", warns)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/imgimport/ -run TestParseImageConfig -v`
Expected: FAIL — `undefined: inspectImage`, `undefined: parseImageConfig`.

- [ ] **Step 3: Write `podman.go`**

`internal/imgimport/podman.go`:

```go
// Package imgimport turns an OCI container image into a Moat rootfs template.
// It pulls or loads the image with podman, records its digest, reads its
// config, exports the filesystem, adds the guest agent, and writes an ext4
// file. It needs no root. There is no loop mount.
package imgimport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// inspectImage is the subset of `podman inspect` output this package reads.
type inspectImage struct {
	ID           string   `json:"Id"`
	RepoDigests  []string `json:"RepoDigests"`
	Architecture string   `json:"Architecture"`
	Os           string   `json:"Os"`
	Config       struct {
		Env        []string `json:"Env"`
		Cmd        []string `json:"Cmd"`
		Entrypoint []string `json:"Entrypoint"`
		WorkingDir string   `json:"WorkingDir"`
	} `json:"Config"`
}

// checkPodman reports whether podman is on PATH.
func checkPodman() error {
	if _, err := exec.LookPath("podman"); err != nil {
		return fmt.Errorf("podman is required for template import")
	}
	return nil
}

// podmanPull fetches the linux/amd64 image for ref into local storage.
func podmanPull(ctx context.Context, ref string) error {
	out, err := exec.CommandContext(ctx, "podman", "pull", "--arch", "amd64", "--os", "linux", ref).CombinedOutput()
	if err != nil {
		return fmt.Errorf("podman pull %s: %v: %s", ref, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// podmanLoad imports an image archive and returns the loaded reference.
func podmanLoad(ctx context.Context, archive string) (string, error) {
	out, err := exec.CommandContext(ctx, "podman", "load", "-i", archive).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("podman load %s: %v: %s", archive, err, strings.TrimSpace(string(out)))
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if r, ok := strings.CutPrefix(line, "Loaded image: "); ok {
			return strings.TrimSpace(r), nil
		}
		if r, ok := strings.CutPrefix(line, "Loaded image(s): "); ok {
			return strings.TrimSpace(r), nil
		}
	}
	return "", fmt.Errorf("podman load %s: no image reference in output: %s", archive, strings.TrimSpace(string(out)))
}

// podmanInspect reads the image metadata for ref.
func podmanInspect(ctx context.Context, ref string) (inspectImage, error) {
	out, err := exec.CommandContext(ctx, "podman", "inspect", "--type", "image", ref).Output()
	if err != nil {
		return inspectImage{}, fmt.Errorf("podman inspect %s: %v", ref, err)
	}
	var arr []inspectImage
	if err := json.Unmarshal(out, &arr); err != nil {
		return inspectImage{}, fmt.Errorf("parse podman inspect %s: %w", ref, err)
	}
	if len(arr) == 0 {
		return inspectImage{}, fmt.Errorf("podman inspect %s: no result", ref)
	}
	return arr[0], nil
}

// parseImageConfig turns an inspected image into template exec defaults.
func parseImageConfig(im inspectImage) (env map[string]string, dir string, defaultArgv []string, warnings []string) {
	env = map[string]string{}
	for _, kv := range im.Config.Env {
		k, v, ok := strings.Cut(kv, "=")
		if ok {
			env[k] = v
		}
	}
	dir = im.Config.WorkingDir
	argv := append(append([]string{}, im.Config.Entrypoint...), im.Config.Cmd...)
	if len(argv) > 0 {
		defaultArgv = argv
	} else {
		warnings = append(warnings, "the image has no ENTRYPOINT or CMD, so every exec must send argv")
	}
	return env, dir, defaultArgv, warnings
}

// podmanExportRootfs writes the image filesystem into destDir.
func podmanExportRootfs(ctx context.Context, ref, destDir string) error {
	cidOut, err := exec.CommandContext(ctx, "podman", "create", ref, "/bin/true").Output()
	if err != nil {
		return fmt.Errorf("podman create %s: %v", ref, err)
	}
	cid := strings.TrimSpace(string(cidOut))
	defer exec.Command("podman", "rm", "-f", cid).Run()

	export := exec.CommandContext(ctx, "podman", "export", cid)
	untar := exec.CommandContext(ctx, "tar", "-x", "-C", destDir, "-f", "-")
	pipe, err := export.StdoutPipe()
	if err != nil {
		return err
	}
	untar.Stdin = pipe
	var untarErr bytes.Buffer
	untar.Stderr = &untarErr
	if err := untar.Start(); err != nil {
		return err
	}
	if err := export.Run(); err != nil {
		return fmt.Errorf("podman export %s: %v", cid, err)
	}
	if err := untar.Wait(); err != nil {
		return fmt.Errorf("extract %s: %v: %s", cid, err, strings.TrimSpace(untarErr.String()))
	}
	return nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/imgimport/ -run TestParseImageConfig -v`
Expected: PASS (both cases).

- [ ] **Step 5: Verify the package builds and vets**

Run: `go build ./internal/imgimport/ && go vet ./internal/imgimport/`
Expected: no output.

- [ ] **Step 6: Commit**

```bash
git add internal/imgimport/podman.go internal/imgimport/imgimport_test.go
git commit -m "imgimport: podman subprocess layer and image-config parsing"
```

---

## Task 2: `internal/imgimport` — rootfs assembly

**Files:**
- Create: `internal/imgimport/rootfs.go`
- Modify: `internal/imgimport/imgimport_test.go`

**Interfaces:**
- Consumes: nothing from Task 1.
- Produces:
  - `func uuidFromName(name string) string` — a stable UUID-format string from `sha256(name)[0:16]`.
  - `func seedFromName(name string) string` — a stable UUID-format string from `sha256(name)[16:32]`.
  - `func autoSizeBytes(treeBytes int64) int64` — `treeBytes * 3 / 2`, rounded up to a whole MiB, floor `64 << 20`.
  - `func assembleRootfs(root, agentPath, outPath, label string, sourceDateEpoch int64, sizeBytes int64) (usedBytes int64, warnings []string, err error)` — installs the agent at `root/sbin/guest-agent`, makes the mount-point dirs, writes `root/etc/passwd` when absent, warns when `root/bin/sh` is missing, normalises times, runs `mkfs.ext4` to `outPath + ".tmp"`, renames to `outPath`. `sizeBytes == 0` means auto.
  - `func dirSizeBytes(root string) (int64, error)` — `du -sb <root>`, parses the leading integer.

- [ ] **Step 1: Write the failing tests**

Append to `internal/imgimport/imgimport_test.go`:

```go
func TestUUIDAndSeedFromNameAreStableAndDistinct(t *testing.T) {
	a1, a2 := uuidFromName("python"), uuidFromName("python")
	if a1 != a2 {
		t.Fatalf("uuidFromName not stable: %q vs %q", a1, a2)
	}
	if uuidFromName("python") == uuidFromName("node") {
		t.Fatal("uuidFromName collided for different names")
	}
	if uuidFromName("python") == seedFromName("python") {
		t.Fatal("uuid and seed must differ")
	}
	// shape: 8-4-4-4-12 hex
	if m := len(a1); m != 36 {
		t.Fatalf("uuid length = %d, want 36 (%q)", m, a1)
	}
}

func TestAutoSizeBytes(t *testing.T) {
	cases := []struct{ in, want int64 }{
		{0, 64 << 20},
		{1 << 20, 64 << 20},           // tiny -> floor
		{200 << 20, 300 << 20},        // 200 MiB -> 300 MiB
		{201*1024*1024 + 1, 302 << 20}, // rounds up to a whole MiB
	}
	for _, c := range cases {
		if got := autoSizeBytes(c.in); got != c.want {
			t.Errorf("autoSizeBytes(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/imgimport/ -run 'TestUUIDAndSeed|TestAutoSize' -v`
Expected: FAIL — `undefined: uuidFromName`, `undefined: autoSizeBytes`.

- [ ] **Step 3: Write `rootfs.go`**

`internal/imgimport/rootfs.go`:

```go
package imgimport

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const defaultSourceDateEpoch int64 = 1788134400 // 2026-08-31 00:00:00 UTC

// uuidFromName returns a stable filesystem UUID for a template name.
func uuidFromName(name string) string {
	h := sha256.Sum256([]byte("moat-template-uuid:" + name))
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

// seedFromName returns a stable directory hash seed for a template name.
func seedFromName(name string) string {
	h := sha256.Sum256([]byte("moat-template-seed:" + name))
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

// autoSizeBytes picks an ext4 size from the userland size.
func autoSizeBytes(treeBytes int64) int64 {
	const mib = 1 << 20
	n := treeBytes * 3 / 2
	n = (n + mib - 1) / mib * mib
	if n < 64*mib {
		n = 64 * mib
	}
	return n
}

// dirSizeBytes returns the apparent size of the tree under root.
func dirSizeBytes(root string) (int64, error) {
	out, err := exec.Command("du", "-sb", root).Output()
	if err != nil {
		return 0, fmt.Errorf("du %s: %v", root, err)
	}
	field, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\t")
	n, err := strconv.ParseInt(strings.TrimSpace(field), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse du output %q: %w", string(out), err)
	}
	return n, nil
}

// assembleRootfs adds the agent and the scaffolding to root, then writes an
// ext4 file at outPath. sizeBytes 0 means "size from the tree".
func assembleRootfs(root, agentPath, outPath, label string, sourceDateEpoch, sizeBytes int64) (int64, []string, error) {
	var warnings []string
	if sourceDateEpoch == 0 {
		sourceDateEpoch = defaultSourceDateEpoch
	}

	if err := installAgent(agentPath, filepath.Join(root, "sbin", "guest-agent")); err != nil {
		return 0, nil, err
	}
	for _, d := range []string{"proc", "sys", "dev", "tmp", "run", "work"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return 0, nil, err
		}
	}
	passwd := filepath.Join(root, "etc", "passwd")
	if _, err := os.Stat(passwd); err != nil {
		_ = os.MkdirAll(filepath.Join(root, "etc"), 0o755)
		_ = os.WriteFile(passwd, []byte("root:x:0:0:root:/root:/bin/sh\n"), 0o644)
	}
	if _, err := os.Stat(filepath.Join(root, "bin", "sh")); err != nil {
		warnings = append(warnings, "the image has no /bin/sh, so only absolute-path execs work")
	}

	epoch := strconv.FormatInt(sourceDateEpoch, 10)
	if out, err := exec.Command("find", root, "-exec", "touch", "--no-dereference", "--date=@"+epoch, "{}", "+").CombinedOutput(); err != nil {
		return 0, nil, fmt.Errorf("normalise timestamps: %v: %s", err, strings.TrimSpace(string(out)))
	}

	used, err := dirSizeBytes(root)
	if err != nil {
		return 0, nil, err
	}
	size := sizeBytes
	if size == 0 {
		size = autoSizeBytes(used)
	}

	tmp := outPath + ".tmp"
	_ = os.Remove(tmp)
	cmd := exec.Command("mkfs.ext4", "-q", "-F", "-L", label,
		"-U", uuidFromName(label),
		"-E", "hash_seed="+seedFromName(label)+",lazy_itable_init=0,lazy_journal_init=0",
		"-d", root, tmp, strconv.FormatInt(size/1024, 10)+"k")
	cmd.Env = append(os.Environ(), "E2FSPROGS_FAKE_TIME="+epoch)
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(tmp)
		return 0, warnings, fmt.Errorf("mkfs.ext4 (%d bytes may be too small, pass --size): %v: %s",
			size, err, strings.TrimSpace(string(out)))
	}
	if err := os.Rename(tmp, outPath); err != nil {
		return 0, warnings, err
	}
	return size, warnings, nil
}

// installAgent copies the static guest agent to dst with mode 0755.
func installAgent(src, dst string) error {
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("guest agent not found at %s (run: make agent, or pass --agent)", src)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := out.ReadFrom(in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/imgimport/ -run 'TestUUIDAndSeed|TestAutoSize|TestParseImageConfig' -v`
Expected: PASS.

- [ ] **Step 5: Build and vet**

Run: `go build ./internal/imgimport/ && go vet ./internal/imgimport/`
Expected: no output.

- [ ] **Step 6: Commit**

```bash
git add internal/imgimport/rootfs.go internal/imgimport/imgimport_test.go
git commit -m "imgimport: rootfs assembly, deterministic UUID and seed, auto-size"
```

---

## Task 3: `internal/imgimport` — `Import` orchestration

**Files:**
- Create: `internal/imgimport/imgimport.go`
- Create: `internal/imgimport/imgimport_integration_test.go`

**Interfaces:**
- Consumes: `checkPodman`, `podmanPull`, `podmanLoad`, `podmanInspect`, `parseImageConfig`, `podmanExportRootfs` (Task 1); `assembleRootfs` (Task 2).
- Produces:
  - `type Source struct { Ref string; Archive string }`
  - `type Options struct { Name string; SizeBytes int64; AgentPath string; OutDir string; SourceDateEpoch int64 }`
  - `type Result struct { RootFSPath string; Source string; Env map[string]string; Dir string; DefaultArgv []string; SizeBytes int64; Warnings []string; PackagesManifest []byte }`
  - `func Import(ctx context.Context, src Source, opts Options) (*Result, error)`

- [ ] **Step 1: Write the failing integration test**

`internal/imgimport/imgimport_integration_test.go`:

```go
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
	if fi, err := os.Stat(res.RootFSPath); err != nil || fi.Size() < 1<<20 {
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -tags integration ./internal/imgimport/ -run TestImport -v`
Expected: FAIL — `undefined: imgimport.Import`, `undefined: imgimport.Source`.

- [ ] **Step 3: Write `imgimport.go`**

`internal/imgimport/imgimport.go`:

```go
package imgimport

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Source names the image to import. Set exactly one field.
type Source struct {
	Ref     string // a registry reference, for example "python:3.12"
	Archive string // a path to a docker-save or OCI archive
}

// Options controls the import.
type Options struct {
	Name            string // template name and ext4 label
	SizeBytes       int64  // ext4 size; 0 means "size from the image"
	AgentPath       string // the guest-agent binary; required
	OutDir          string // the directory for <Name>-rootfs.ext4; required
	SourceDateEpoch int64  // 0 means the default
}

// Result reports what the import produced.
type Result struct {
	RootFSPath       string
	Source           string // "<repo>@sha256:..." or the image id
	Env              map[string]string
	Dir              string
	DefaultArgv      []string
	SizeBytes        int64
	Warnings         []string
	PackagesManifest []byte
}

// Import builds a Moat rootfs template from an OCI image.
func Import(ctx context.Context, src Source, opts Options) (*Result, error) {
	if err := checkPodman(); err != nil {
		return nil, err
	}
	if opts.Name == "" || opts.OutDir == "" || opts.AgentPath == "" {
		return nil, fmt.Errorf("Import needs Name, OutDir, and AgentPath")
	}

	ref := src.Ref
	if src.Archive != "" {
		loaded, err := podmanLoad(ctx, src.Archive)
		if err != nil {
			return nil, err
		}
		ref = loaded
	} else {
		if err := podmanPull(ctx, ref); err != nil {
			return nil, err
		}
	}

	im, err := podmanInspect(ctx, ref)
	if err != nil {
		return nil, err
	}
	if im.Architecture != "amd64" || (im.Os != "" && im.Os != "linux") {
		return nil, fmt.Errorf("image %s is %s/%s, need linux/amd64", ref, im.Os, im.Architecture)
	}

	res := &Result{}
	res.Env, res.Dir, res.DefaultArgv, res.Warnings = parseImageConfig(im)
	switch {
	case len(im.RepoDigests) > 0:
		res.Source = im.RepoDigests[0]
	default:
		res.Source = im.ID
		res.Warnings = append(res.Warnings, "the image has no registry digest, recording the local image id")
	}

	work, err := os.MkdirTemp("", "moat-imgimport-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	root := filepath.Join(work, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	if err := podmanExportRootfs(ctx, ref, root); err != nil {
		return nil, err
	}

	if b, err := os.ReadFile(filepath.Join(root, "lib", "apk", "db", "installed")); err == nil {
		res.PackagesManifest = b
	} else if b, err := os.ReadFile(filepath.Join(root, "var", "lib", "dpkg", "status")); err == nil {
		res.PackagesManifest = b
	}

	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return nil, err
	}
	out := filepath.Join(opts.OutDir, opts.Name+"-rootfs.ext4")
	size, warns, err := assembleRootfs(root, opts.AgentPath, out, opts.Name, opts.SourceDateEpoch, opts.SizeBytes)
	if err != nil {
		return nil, err
	}
	res.RootFSPath = out
	res.SizeBytes = size
	res.Warnings = append(res.Warnings, warns...)
	res.Warnings = dedupeStrings(res.Warnings)
	return res, nil
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// FormatWarnings joins warnings for a one-line log.
func FormatWarnings(w []string) string { return strings.Join(w, "; ") }
```

- [ ] **Step 4: Run the integration test to verify it passes**

Run: `make agent && go test -tags integration ./internal/imgimport/ -run TestImport -v`
Expected: PASS (`TestImportBuildsRootfsAndReadsConfig`, `TestImportFromArchive`). Skips only when `podman` or `debugfs` is absent.

- [ ] **Step 5: Run the unit tests and vet the whole package**

Run: `go test ./internal/imgimport/ && go vet ./internal/imgimport/`
Expected: PASS, no vet output.

- [ ] **Step 6: Commit**

```bash
git add internal/imgimport/imgimport.go internal/imgimport/imgimport_integration_test.go
git commit -m "imgimport: Import orchestration for registry and archive sources"
```

---

## Task 4: `server.Template` fields, `Sandbox.tpl`, and the `mergeExec` helper

**Files:**
- Modify: `internal/server/sandbox.go:24-34` (the `Template` struct) and `:37-53` (the `Sandbox` struct)
- Create: `internal/server/exec_merge.go`
- Create: `internal/server/exec_merge_test.go`

**Interfaces:**
- Consumes: `execReq` (defined in `internal/server/server.go`, same package).
- Produces:
  - `Template` gains: `Env map[string]string`, `Dir string`, `DefaultArgv []string`, `Source string`.
  - `Sandbox` gains an unexported field `tpl Template`.
  - `func mergeExec(tpl Template, req execReq) (argv []string, env map[string]string, dir, argvSource string, err error)`
  - `var errNoArgv error`

- [ ] **Step 1: Write the failing test**

`internal/server/exec_merge_test.go`:

```go
package server

import (
	"reflect"
	"testing"
)

func TestMergeExecArgvFallback(t *testing.T) {
	tpl := Template{DefaultArgv: []string{"python3", "app.py"}}

	argv, _, _, src, err := mergeExec(tpl, execReq{})
	if err != nil {
		t.Fatal(err)
	}
	if src != "template_default" || !reflect.DeepEqual(argv, []string{"python3", "app.py"}) {
		t.Fatalf("argv = %v src = %q", argv, src)
	}

	argv, _, _, src, err = mergeExec(tpl, execReq{Argv: []string{"ls"}})
	if err != nil || src != "request" || !reflect.DeepEqual(argv, []string{"ls"}) {
		t.Fatalf("request argv should win: %v %q %v", argv, src, err)
	}
}

func TestMergeExecNoArgvNoDefault(t *testing.T) {
	if _, _, _, _, err := mergeExec(Template{}, execReq{}); err != errNoArgv {
		t.Fatalf("err = %v, want errNoArgv", err)
	}
}

func TestMergeExecEnvAndDir(t *testing.T) {
	tpl := Template{
		Env: map[string]string{"PATH": "/img/bin", "LANG": "C"},
		Dir: "/srv",
	}
	req := execReq{
		Argv: []string{"true"},
		Env:  map[string]string{"PATH": "/req/bin"},
	}
	_, env, dir, _, err := mergeExec(tpl, req)
	if err != nil {
		t.Fatal(err)
	}
	if env["PATH"] != "/req/bin" {
		t.Errorf("request env must override: PATH = %q", env["PATH"])
	}
	if env["LANG"] != "C" {
		t.Errorf("template env must survive: LANG = %q", env["LANG"])
	}
	if dir != "/srv" {
		t.Errorf("dir = %q, want /srv from the template", dir)
	}

	_, _, dir, _, _ = mergeExec(tpl, execReq{Argv: []string{"true"}, Dir: "/"})
	if dir != "/" {
		t.Errorf("request dir must win: %q", dir)
	}
}

func TestMergeExecDoesNotMutateInputs(t *testing.T) {
	tplEnv := map[string]string{"A": "1"}
	tpl := Template{Env: tplEnv}
	req := execReq{Argv: []string{"true"}, Env: map[string]string{"B": "2"}}
	_, env, _, _, _ := mergeExec(tpl, req)
	env["C"] = "3"
	if _, ok := tplEnv["C"]; ok {
		t.Error("mergeExec mutated the template env map")
	}
	if _, ok := req.Env["C"]; ok {
		t.Error("mergeExec mutated the request env map")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/server/ -run TestMergeExec -v`
Expected: FAIL — `undefined: mergeExec`, `undefined: errNoArgv`, `Template` has no field `DefaultArgv`.

- [ ] **Step 3: Add the `Template` and `Sandbox` fields**

In `internal/server/sandbox.go`, extend `Template`:

```go
// Template names a kernel and rootfs a sandbox can start from, plus the
// per-sandbox resource caps and the exec defaults from an imported image.
type Template struct {
	Name   string
	Kernel string
	RootFS string
	VCPUs  int64
	MemMiB int64
	Limits cgroup.Limits
	DiskBW int64 // rootfs bytes/sec, 0 = no limit
	NetBW  int64 // network bytes/sec, 0 = no limit

	// Exec defaults. An imported template fills these from the image config.
	// A request value always wins over the template value.
	Env         map[string]string // added under the request env
	Dir         string            // working directory when the request sends none
	DefaultArgv []string          // argv when the request sends none
	Source      string            // "<repo>@sha256:...", informational
}
```

In the same file, add one field to `Sandbox`:

```go
	Networked bool
	GuestIP   string

	tpl Template // the template this sandbox started from

	// Exactly one of lease / inst is set.
```

- [ ] **Step 4: Write `exec_merge.go`**

`internal/server/exec_merge.go`:

```go
package server

import "errors"

// errNoArgv reports that an exec request sent no argv and the template has no
// default.
var errNoArgv = errors.New("argv is required and the template has no default_argv")

// mergeExec folds a template's exec defaults under a request. A request value
// always wins. argvSource is "request" or "template_default".
func mergeExec(tpl Template, req execReq) (argv []string, env map[string]string, dir, argvSource string, err error) {
	argv = req.Argv
	argvSource = "request"
	if len(argv) == 0 {
		argv = tpl.DefaultArgv
		argvSource = "template_default"
	}
	if len(argv) == 0 {
		return nil, nil, "", "", errNoArgv
	}

	env = make(map[string]string, len(tpl.Env)+len(req.Env))
	for k, v := range tpl.Env {
		env[k] = v
	}
	for k, v := range req.Env {
		env[k] = v
	}

	dir = req.Dir
	if dir == "" {
		dir = tpl.Dir
	}
	return argv, env, dir, argvSource, nil
}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/server/ -run TestMergeExec -v`
Expected: PASS (all four cases).

- [ ] **Step 6: Build and vet the package**

Run: `go build ./... && go vet ./internal/server/`
Expected: no output. (`handleExec` still compiles — it does not use the new fields yet.)

- [ ] **Step 7: Commit**

```bash
git add internal/server/sandbox.go internal/server/exec_merge.go internal/server/exec_merge_test.go
git commit -m "server: Template exec-default fields and a mergeExec helper"
```

---

## Task 5: `moatd.json` schema — the four new template fields

**Files:**
- Modify: `cmd/moatd/config.go:40-51` (`fileTemplate`) and `:140-153` (the `server.Template{...}` literal)
- Modify: `cmd/moatd/config_test.go`

**Interfaces:**
- Consumes: `server.Template.Env` / `.Dir` / `.DefaultArgv` / `.Source` (Task 4).
- Produces: `fileTemplate` gains `Env map[string]string` (`json:"env"`), `Workdir string` (`json:"workdir"`), `DefaultArgv []string` (`json:"default_argv"`), `Source string` (`json:"source"`). `loadConfigFile` copies them onto `server.Template`.

- [ ] **Step 1: Write the failing test**

Append to `cmd/moatd/config_test.go` (match the file's existing helper style — it already writes temp config files and calls `loadConfigFile`):

```go
func TestLoadConfigTemplateExecDefaults(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "moatd.json")
	body := `{
	  "templates": [
	    { "name": "python", "rootfs": "/data/python-rootfs.ext4",
	      "env": {"PATH": "/usr/local/bin:/usr/bin", "PYTHONUNBUFFERED": "1"},
	      "workdir": "/app",
	      "default_argv": ["python3"],
	      "source": "docker.io/library/python@sha256:abc" }
	  ],
	  "tokens": [ { "id": "t", "secret": "s" } ]
	}`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfigFile(p, "/data/vmlinux", "/data/rootfs.ext4")
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
	dir := t.TempDir()
	p := filepath.Join(dir, "moatd.json")
	body := `{"templates":[{"name":"default"}],"tokens":[{"id":"t","secret":"s"}]}`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfigFile(p, "/k", "/r")
	if err != nil {
		t.Fatal(err)
	}
	tpl := cfg.Templates[0]
	if tpl.Env != nil || tpl.Dir != "" || tpl.DefaultArgv != nil {
		t.Errorf("unset exec defaults must stay zero: %+v", tpl)
	}
}
```

(If `config_test.go` does not import `path/filepath` / `os`, add them.)

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cmd/moatd/ -run TestLoadConfigTemplate -v`
Expected: FAIL — `unknown field "env"` from `DisallowUnknownFields`, and `tpl.Dir` undefined-ish assertions.

- [ ] **Step 3: Add the fields and the copy**

In `cmd/moatd/config.go`, extend `fileTemplate`:

```go
type fileTemplate struct {
	Name      string  `json:"name"`
	Kernel    string  `json:"kernel"`
	RootFS    string  `json:"rootfs"`
	VCPUs     int64   `json:"vcpus"`
	MemMiB    int64   `json:"mem_mib"`
	CPUMax    float64 `json:"cpu_max"`
	MemMaxMiB int64   `json:"mem_max_mib"`
	PidsMax   int64   `json:"pids_max"`
	DiskBW    int64   `json:"disk_bw"`
	NetBW     int64   `json:"net_bw"`

	// Exec defaults from an imported image. All optional.
	Env         map[string]string `json:"env"`
	Workdir     string            `json:"workdir"`
	DefaultArgv []string          `json:"default_argv"`
	Source      string            `json:"source"`
}
```

In `loadConfigFile`, extend the `server.Template{...}` literal:

```go
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
			DiskBW:      ft.DiskBW,
			NetBW:       ft.NetBW,
			Env:         ft.Env,
			Dir:         ft.Workdir,
			DefaultArgv: ft.DefaultArgv,
			Source:      ft.Source,
		}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./cmd/moatd/ -run TestLoadConfigTemplate -v`
Expected: PASS (both cases).

- [ ] **Step 5: Run the whole `cmd/moatd` test package**

Run: `go test ./cmd/moatd/`
Expected: PASS — no existing config test regressed.

- [ ] **Step 6: Commit**

```bash
git add cmd/moatd/config.go cmd/moatd/config_test.go
git commit -m "moatd config: env, workdir, default_argv, source on a template entry"
```

---

## Task 6: Wire `mergeExec` into `handleCreate` and `handleExec`

**Files:**
- Modify: `internal/server/server.go` — `handleCreate` (~line 513, the `sb := &Sandbox{...}` literal) and `handleExec` (~lines 682-795)
- Modify: `internal/server/server_integration_test.go` — add `TestExecTemplateDefaults`

**Interfaces:**
- Consumes: `mergeExec`, `errNoArgv`, `Sandbox.tpl`, `Template.Env/Dir/DefaultArgv` (Task 4).
- Produces: no new exported symbols. Behaviour change: an exec merges the sandbox template's defaults under the request; the exec audit event gains `"argv_source"` and its `env_keys` is the union of template and request keys.

- [ ] **Step 1: Write the failing integration test**

Append to `internal/server/server_integration_test.go` (uses the existing `copyRootFS`, `config.NewMachine`, and NDJSON helpers in that file):

```go
func TestExecTemplateDefaults(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("no /dev/kvm")
	}
	m, err := config.NewMachine("srvtest-tmpldef")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Skipf("images missing (run: make images): %v", err)
	}
	log := logrus.New()
	log.SetLevel(logrus.WarnLevel)
	srv, err := server.New(server.Config{
		Templates: []server.Template{{
			Name: "default", Kernel: m.KernelImagePath, RootFS: copyRootFS(t, m.RootFSPath),
			VCPUs: 1, MemMiB: 256,
			Env:         map[string]string{"MOAT_TMPL_VAR": "hello"},
			Dir:         "/tmp",
			DefaultArgv: []string{"echo", "default-cmd"},
		}},
		Tokens: map[string]string{"t1": "secret1"},
	}, log)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	defer srv.Shutdown(context.Background())

	id := createSandbox(t, ts, "secret1") // existing helper in this file

	// no dir, no argv override -> runs in /tmp and sees the template env
	out, code := execCollect(t, ts, "secret1", id, execBody{Argv: []string{"sh", "-c", "pwd; echo $MOAT_TMPL_VAR"}})
	if code != 0 || !strings.Contains(out, "/tmp") || !strings.Contains(out, "hello") {
		t.Fatalf("template defaults not applied: code=%d out=%q", code, out)
	}

	// request dir and env win
	out, _ = execCollect(t, ts, "secret1", id, execBody{
		Argv: []string{"sh", "-c", "pwd; echo $MOAT_TMPL_VAR"},
		Dir:  "/", Env: map[string]string{"MOAT_TMPL_VAR": "override"},
	})
	if !strings.Contains(out, "override") || strings.Contains(out, "/tmp") {
		t.Fatalf("request values did not win: out=%q", out)
	}

	// empty argv -> the template default runs
	out, code = execCollect(t, ts, "secret1", id, execBody{})
	if code != 0 || !strings.Contains(out, "default-cmd") {
		t.Fatalf("default_argv did not run: code=%d out=%q", code, out)
	}
}
```

> **Note for the implementer:** this test reuses helpers already in
> `server_integration_test.go`. Check their exact names before writing —
> the file has a sandbox-create helper and an exec-NDJSON-collect helper.
> If the names differ (e.g. `mkSandbox`, `runExec`), use those and shape
> `execBody` to the existing request struct. Do not add new helpers if
> equivalents exist.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -tags integration ./internal/server/ -run TestExecTemplateDefaults -v`
Expected: FAIL — `Template` literal has the fields (Task 4), but `handleExec` ignores them, so the `pwd` is `/` and `$MOAT_TMPL_VAR` is empty; the empty-argv case returns `400`.

- [ ] **Step 3: Set `sb.tpl` in `handleCreate`**

In `internal/server/server.go`, change the sandbox literal:

```go
	id := newSandboxID()
	start := time.Now()
	sb := &Sandbox{ID: id, Template: name, TokenID: tid, CreatedAt: start,
		ExpiresAt: start.Add(ttl), Networked: req.Networked, tpl: tpl}
```

- [ ] **Step 4: Rework the argv check and the exec call in `handleExec`**

In `internal/server/server.go` `handleExec`, **remove** the early block:

```go
	if len(req.Argv) == 0 {
		writeErr(w, http.StatusBadRequest, "argv is required")
		return
	}
```

After the secret-firewall block and **before** the "Networked sandboxes must send outbound HTTP through the egress proxy" block, insert:

```go
	// Fold the sandbox template's exec defaults under the request. A request
	// value always wins.
	argv, env, dir, argvSource, err := mergeExec(sb.tpl, req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
```

Change the proxy block to mutate `env` (not `req.Env`):

```go
	if sb.net != nil && s.cfg.EgressPort > 0 {
		proxy := fmt.Sprintf("http://%s:%d", sb.net.VethHostIP, s.cfg.EgressPort)
		for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
			env[k] = proxy
		}
		env["NO_PROXY"] = "localhost,127.0.0.1"
	}
```

Change the `Exec` call to pass the merged values:

```go
	res, err := sb.client().Exec(r.Context(), agentclient.ExecOptions{
		Argv:    argv,
		Env:     env,
		Dir:     dir,
		Stdin:   stdin,
		TTY:     req.TTY,
		Timeout: time.Duration(req.TimeoutMS) * time.Millisecond,
	}, streamWriter{send, "stdout"}, streamWriter{send, "stderr"})
```

- [ ] **Step 5: Update the exec audit attrs**

Still in `handleExec`, change the audit assembly so it reflects what ran:

```go
	loggedArgv := clampArgv(argv, 4096)
	envKeys := make([]string, 0, len(env))
	for k := range env {
		if k == "HTTP_PROXY" || k == "HTTPS_PROXY" || k == "http_proxy" || k == "https_proxy" || k == "NO_PROXY" {
			continue // the server injects these, they are not caller data
		}
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	execAttrs := map[string]any{
		"argv": loggedArgv, "argv_source": argvSource, "env_keys": envKeys,
		"dir": dir, "tty": req.TTY,
		"dur_ms": time.Since(start).Milliseconds(),
	}
```

(Leave the `err`/`res` branch of `execAttrs` below it unchanged.)

- [ ] **Step 6: Run the integration test to verify it passes**

Run: `go test -tags integration ./internal/server/ -run TestExecTemplateDefaults -v`
Expected: PASS.

- [ ] **Step 7: Run the merge unit tests and the full server integration suite**

Run: `go test ./internal/server/ -run TestMergeExec && go test -tags integration ./internal/server/`
Expected: PASS. Watch `TestAPILifecycle`, `TestQuota`, and the audit tests — the `env_keys` union and the new `argv_source` key must not break their assertions. If an audit test asserts an exact attrs map, add `argv_source` to its expectation.

- [ ] **Step 8: Commit**

```bash
git add internal/server/server.go internal/server/server_integration_test.go
git commit -m "server: apply template exec defaults in handleExec"
```

---

## Task 7: `moat template import` CLI

**Files:**
- Create: `cmd/moat/template_import.go`
- Create: `cmd/moat/template_import_test.go`
- Modify: `cmd/moat/template.go` — add the `import` case and expand the help text

**Interfaces:**
- Consumes: `imgimport.Import`, `imgimport.Source`, `imgimport.Options`, `imgimport.Result` (Task 3); `config.ArtifactsDir` (existing).
- Produces:
  - `func templateImport(args []string) int`
  - `func deriveName(ref string) (string, error)` — basename of the repo, tag and digest stripped, lower-cased; error when it is empty, over 16 chars, or not `[a-z0-9-]+`.
  - `type templateEntry struct` with `json` tags matching `fileTemplate` (`name`, `rootfs`, `env`, `workdir`, `default_argv`, `source`).
  - `func registerTemplate(configPath string, e templateEntry) error` — append to the `templates` array of a `moatd.json`, reject a duplicate `name`, write atomically. `configPath == ""` is a programming error here (the caller writes the sidecar instead).
  - `func writeSidecar(dataDir string, e templateEntry) (string, error)` — write `<dataDir>/<name>.template.json`, return the path.

- [ ] **Step 1: Write the failing unit tests**

`cmd/moat/template_import_test.go`:

```go
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDeriveName(t *testing.T) {
	cases := map[string]string{
		"python:3.12":                 "python",
		"ghcr.io/org/app:v2":          "app",
		"docker.io/library/python":    "python",
		"registry.io/team/svc@sha256:deadbeef": "svc",
		"NODE:20":                     "node",
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./cmd/moat/ -run 'TestDeriveName|TestRegisterTemplate|TestWriteSidecar|TestTemplateImportDryRun' -v`
Expected: FAIL — `undefined: deriveName`, `templateImport`, `registerTemplate`, `writeSidecar`, `templateEntry`.

- [ ] **Step 3: Write `template_import.go`**

`cmd/moat/template_import.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/M30W-M1X/moat/internal/config"
	"github.com/M30W-M1X/moat/internal/imgimport"
)

// templateEntry is one moatd.json "templates" element that an import writes.
type templateEntry struct {
	Name        string            `json:"name"`
	RootFS      string            `json:"rootfs"`
	Env         map[string]string `json:"env,omitempty"`
	Workdir     string            `json:"workdir,omitempty"`
	DefaultArgv []string          `json:"default_argv,omitempty"`
	Source      string            `json:"source,omitempty"`
}

var nameRe = regexp.MustCompile(`^[a-z0-9-]+$`)

// deriveName picks a template name from an image reference.
func deriveName(ref string) (string, error) {
	s := ref
	if i := strings.Index(s, "@"); i >= 0 {
		s = s[:i]
	}
	slash := strings.LastIndex(s, "/")
	if c := strings.LastIndex(s, ":"); c > slash {
		s = s[:c]
	}
	if slash := strings.LastIndex(s, "/"); slash >= 0 {
		s = s[slash+1:]
	}
	s = strings.ToLower(s)
	if s == "" || len(s) > 16 || !nameRe.MatchString(s) {
		return "", fmt.Errorf("cannot derive a valid name from %q, pass --name (lower-case, [a-z0-9-], <=16)", ref)
	}
	return s, nil
}

// templateImport implements `moat template import`.
func templateImport(args []string) int {
	fs := flag.NewFlagSet("template import", flag.ContinueOnError)
	name := fs.String("name", "", "template name (default: derived from the image reference)")
	archive := fs.String("from-archive", "", "import this docker-save or OCI archive instead of a pull")
	sizeStr := fs.String("size", "", "ext4 size, for example 512M or 2G (default: from the image)")
	dataDir := fs.String("data", config.ArtifactsDir(), "directory for <name>-rootfs.ext4")
	agentPath := fs.String("agent", "", "guest-agent binary (default: auto-resolve)")
	cfgPath := fs.String("config", "", "append the template entry to this moatd.json")
	dryRun := fs.Bool("dry-run", false, "print the plan and do nothing")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	var src imgimport.Source
	switch {
	case *archive != "":
		src.Archive = *archive
	case fs.NArg() == 1:
		src.Ref = fs.Arg(0)
	default:
		fmt.Fprintln(os.Stderr, "usage: moat template import <image-ref> [flags]   (or --from-archive FILE)")
		return 2
	}

	tName := *name
	if tName == "" {
		derived, err := deriveName(firstNonEmpty(src.Ref, filepath.Base(src.Archive)))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		tName = derived
	} else if !nameRe.MatchString(tName) || len(tName) > 16 {
		fmt.Fprintf(os.Stderr, "invalid --name %q (lower-case, [a-z0-9-], <=16)\n", tName)
		return 1
	}

	var sizeBytes int64
	if *sizeStr != "" {
		n, err := parseSize(*sizeStr)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		sizeBytes = n
	}

	agent := *agentPath
	if agent == "" {
		agent = resolveAgent(*dataDir)
	}

	rootfsOut := filepath.Join(*dataDir, tName+"-rootfs.ext4")
	if *dryRun {
		fmt.Printf("would import %s\n", firstNonEmpty(src.Ref, src.Archive))
		fmt.Printf("  name    %s\n", tName)
		fmt.Printf("  rootfs  %s\n", rootfsOut)
		fmt.Printf("  agent   %s\n", agent)
		if *cfgPath != "" {
			fmt.Printf("  config  append to %s\n", *cfgPath)
		} else {
			fmt.Printf("  config  write %s\n", filepath.Join(*dataDir, tName+".template.json"))
		}
		return 0
	}

	res, err := imgimport.Import(context.Background(), src, imgimport.Options{
		Name: tName, SizeBytes: sizeBytes, AgentPath: agent, OutDir: *dataDir,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "import:", err)
		return 1
	}

	entry := templateEntry{
		Name: tName, RootFS: res.RootFSPath,
		Env: res.Env, Workdir: res.Dir, DefaultArgv: res.DefaultArgv, Source: res.Source,
	}

	var regMsg string
	if *cfgPath != "" {
		if err := registerTemplate(*cfgPath, entry); err != nil {
			fmt.Fprintln(os.Stderr, "register:", err)
			return 1
		}
		regMsg = "appended to " + *cfgPath
	} else {
		p, err := writeSidecar(*dataDir, entry)
		if err != nil {
			fmt.Fprintln(os.Stderr, "sidecar:", err)
			return 1
		}
		regMsg = "wrote " + p
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{
			"name": tName, "rootfs": res.RootFSPath, "source": res.Source,
			"size_bytes": res.SizeBytes, "env": res.Env, "workdir": res.Dir,
			"default_argv": res.DefaultArgv, "warnings": res.Warnings, "registered": regMsg,
		})
		return 0
	}

	fmt.Printf("imported %s\n", res.Source)
	fmt.Printf("  name         %s\n", tName)
	fmt.Printf("  rootfs       %s (%d MiB)\n", res.RootFSPath, res.SizeBytes>>20)
	if res.Dir != "" {
		fmt.Printf("  workdir      %s\n", res.Dir)
	}
	if len(res.DefaultArgv) > 0 {
		fmt.Printf("  default_argv %s\n", strings.Join(res.DefaultArgv, " "))
	}
	if len(res.Env) > 0 {
		fmt.Printf("  env          %d variables\n", len(res.Env))
	}
	for _, wmsg := range res.Warnings {
		fmt.Printf("  warning      %s\n", wmsg)
	}
	fmt.Printf("  %s\n", regMsg)
	if *cfgPath == "" {
		b, _ := json.MarshalIndent([]templateEntry{entry}, "  ", "  ")
		fmt.Printf("\nAdd this to the \"templates\" array of your moatd.json:\n  %s\n", b)
	}
	return 0
}

// registerTemplate appends e to the templates array of a moatd.json.
func registerTemplate(configPath string, e templateEntry) error {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", configPath, err)
	}
	var tmpls []json.RawMessage
	if r, ok := doc["templates"]; ok {
		if err := json.Unmarshal(r, &tmpls); err != nil {
			return fmt.Errorf("parse templates in %s: %w", configPath, err)
		}
	}
	for _, rt := range tmpls {
		var probe struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(rt, &probe)
		if probe.Name == e.Name {
			return fmt.Errorf("template %q already exists in %s (use --name)", e.Name, configPath)
		}
	}
	added, err := json.Marshal(e)
	if err != nil {
		return err
	}
	tmpls = append(tmpls, added)
	packed, err := json.Marshal(tmpls)
	if err != nil {
		return err
	}
	doc["templates"] = packed
	outBytes, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := configPath + ".tmp"
	if err := os.WriteFile(tmp, append(outBytes, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, configPath)
}

// writeSidecar writes a single template entry beside the rootfs.
func writeSidecar(dataDir string, e templateEntry) (string, error) {
	path := filepath.Join(dataDir, e.Name+".template.json")
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// resolveAgent finds the guest-agent binary for an import.
func resolveAgent(dataDir string) string {
	for _, p := range []string{
		filepath.Join("bin", "guest-agent"),
		filepath.Join(dataDir, "guest-agent"),
		filepath.Join(config.ArtifactsDir(), "guest-agent"),
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return filepath.Join(dataDir, "guest-agent") // let imgimport report the clear error
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// parseSize turns "512M" / "2G" / "1048576" into bytes.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "G"), strings.HasSuffix(s, "g"):
		mult, s = 1<<30, s[:len(s)-1]
	case strings.HasSuffix(s, "M"), strings.HasSuffix(s, "m"):
		mult, s = 1<<20, s[:len(s)-1]
	case strings.HasSuffix(s, "K"), strings.HasSuffix(s, "k"):
		mult, s = 1<<10, s[:len(s)-1]
	}
	var n int64
	if _, err := fmt.Sscan(s, &n); err != nil || n <= 0 {
		return 0, fmt.Errorf("bad --size %q (use 512M, 2G, or a byte count)", s)
	}
	return n * mult, nil
}
```

- [ ] **Step 4: Wire the `import` case into `runTemplate`**

In `cmd/moat/template.go`, add to the `switch sub` in `runTemplate`:

```go
	case "import":
		return templateImport(args[1:])
```

Replace the stub sentence in the help text:

```go
		fmt.Fprint(os.Stderr, `moat template - rootfs templates a sandbox can start from

usage:
  moat template list              show the templates this host knows
  moat template import <ref>      import an OCI image as a template
  moat template import --from-archive FILE
  moat template show <name>       print one template's detail

A template is a kernel plus a rootfs. The daemon reads templates from its
-config file (see docs/moatd.example.json). `+"`"+`moat template import`+"`"+` builds a
rootfs from a container image and records the image's ENV, WORKDIR, and
ENTRYPOINT as exec defaults.
`)
```

- [ ] **Step 5: Run the unit tests to verify they pass**

Run: `go test ./cmd/moat/ -run 'TestDeriveName|TestRegisterTemplate|TestWriteSidecar|TestTemplateImportDryRun' -v`
Expected: PASS.

- [ ] **Step 6: Build and vet**

Run: `go build ./... && go vet ./cmd/moat/`
Expected: no output.

- [ ] **Step 7: Commit**

```bash
git add cmd/moat/template_import.go cmd/moat/template_import_test.go cmd/moat/template.go
git commit -m "moat: template import — build a template from a container image"
```

---

## Task 8: `moat template show` and `template list` sidecar scan

**Files:**
- Create: `cmd/moat/template_show.go`
- Create: `cmd/moat/template_show_test.go`
- Modify: `cmd/moat/template.go` — add the `show` case; `templateList` scans `<data>/*.template.json`

**Interfaces:**
- Consumes: `templateEntry` (Task 7); `config.ArtifactsDir` (existing).
- Produces:
  - `func templateShow(args []string) int`
  - `func loadSidecars(dataDir string) []templateEntry` — read every `<dataDir>/*.template.json`, skip unparseable ones.

- [ ] **Step 1: Write the failing tests**

`cmd/moat/template_show_test.go`:

```go
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./cmd/moat/ -run 'TestLoadSidecars|TestTemplateShow' -v`
Expected: FAIL — `undefined: loadSidecars`, `templateShow`.

- [ ] **Step 3: Write `template_show.go`**

`cmd/moat/template_show.go`:

```go
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/M30W-M1X/moat/internal/config"
)

// loadSidecars reads every <dataDir>/*.template.json entry.
func loadSidecars(dataDir string) []templateEntry {
	matches, _ := filepath.Glob(filepath.Join(dataDir, "*.template.json"))
	var out []templateEntry
	for _, m := range matches {
		b, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		var e templateEntry
		if err := json.Unmarshal(b, &e); err != nil || e.Name == "" {
			continue
		}
		out = append(out, e)
	}
	return out
}

// templateShow implements `moat template show <name>`.
func templateShow(args []string) int {
	fs := flag.NewFlagSet("template show", flag.ContinueOnError)
	dataDir := fs.String("data", config.ArtifactsDir(), "directory that holds <name>.template.json")
	cfgPath := fs.String("config", "", "also look in this moatd.json")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: moat template show <name>")
		return 2
	}
	want := fs.Arg(0)

	for _, e := range loadSidecars(*dataDir) {
		if e.Name == want {
			return printEntry(e, *asJSON)
		}
	}
	if *cfgPath != "" {
		if e, ok := entryFromConfig(*cfgPath, want); ok {
			return printEntry(e, *asJSON)
		}
	}
	if want == "default" {
		return printEntry(templateEntry{
			Name:   "default",
			RootFS: filepath.Join(config.ArtifactsDir(), "rootfs.ext4"),
		}, *asJSON)
	}
	fmt.Fprintf(os.Stderr, "no template %q under %s\n", want, *dataDir)
	return 1
}

func entryFromConfig(path, name string) (templateEntry, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return templateEntry{}, false
	}
	var doc struct {
		Templates []templateEntry `json:"templates"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return templateEntry{}, false
	}
	for _, e := range doc.Templates {
		if e.Name == name {
			return e, true
		}
	}
	return templateEntry{}, false
}

func printEntry(e templateEntry, asJSON bool) int {
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(e)
		return 0
	}
	fmt.Printf("name         %s\n", e.Name)
	fmt.Printf("rootfs       %s\n", e.RootFS)
	if e.Source != "" {
		fmt.Printf("source       %s\n", e.Source)
	}
	if e.Workdir != "" {
		fmt.Printf("workdir      %s\n", e.Workdir)
	}
	if len(e.DefaultArgv) > 0 {
		fmt.Printf("default_argv %s\n", strings.Join(e.DefaultArgv, " "))
	}
	if len(e.Env) > 0 {
		fmt.Printf("env\n")
		for k, v := range e.Env {
			fmt.Printf("  %s=%s\n", k, v)
		}
	}
	return 0
}
```

- [ ] **Step 4: Add the `show` case and extend `templateList`**

In `cmd/moat/template.go` `runTemplate` switch:

```go
	case "show":
		return templateShow(args[1:])
```

In `templateList`, after the `default` line, add:

```go
	for _, e := range loadSidecars(config.ArtifactsDir()) {
		state := "not built"
		if fileExists(e.RootFS) {
			state = "ready"
		}
		src := e.Source
		if src == "" {
			src = "-"
		}
		fmt.Printf("%-10s %-8s rootfs=%s source=%s\n", e.Name, state, e.RootFS, src)
	}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./cmd/moat/ -run 'TestLoadSidecars|TestTemplateShow|TestDeriveName|TestRegisterTemplate|TestWriteSidecar' -v`
Expected: PASS.

- [ ] **Step 6: Build, vet, and run the full `cmd/moat` unit suite**

Run: `go build ./... && go vet ./cmd/moat/ && go test ./cmd/moat/`
Expected: no output, tests PASS.

- [ ] **Step 7: Commit**

```bash
git add cmd/moat/template_show.go cmd/moat/template_show_test.go cmd/moat/template.go
git commit -m "moat: template show and list imported templates"
```

---

## Task 9: Documentation and the example config

**Files:**
- Modify: `docs/moatd.example.json`
- Modify: `build/templates/README.md`
- Modify: `docs/developer-guide.md` (§5 image pipeline, §7 using it)
- Modify: `docs/developer-guide.html` (the matching sections)
- Modify: `README.md`

**Interfaces:** none — documentation only.

- [ ] **Step 1: Add an imported-style template to `docs/moatd.example.json`**

Add one entry to the `templates` array (after `dev-shell`):

```json
    {
      "name": "python",
      "kernel": "/var/lib/moat/vmlinux",
      "rootfs": "/var/lib/moat/python-rootfs.ext4",
      "source": "docker.io/library/python@sha256:REPLACE_WITH_REAL_DIGEST",
      "env": { "PATH": "/usr/local/bin:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG": "C.UTF-8" },
      "workdir": "/",
      "default_argv": ["python3"],
      "vcpus": 2,
      "mem_mib": 1024
    }
```

Run `jq -e . docs/moatd.example.json >/dev/null` to confirm it is valid JSON.

- [ ] **Step 2: Add an "Import an existing image" section to `build/templates/README.md`**

After the "Commands" section, add:

```markdown
## Import an existing image

`moat template import` turns any linux/amd64 container image into a
template. No Dockerfile.

```sh
moat template import python:3.12            # pull and build
moat template import --from-archive img.tar # a docker-save or OCI archive
moat template import ghcr.io/org/app:v2 --name app --config moatd.json
```

It records the image `@sha256` digest, and carries the image's `ENV`,
`WORKDIR`, and `ENTRYPOINT` forward: `moatd` applies them as exec
defaults, and a request value always wins. Without `--config` it writes
`<data>/<name>.template.json` and prints the snippet to paste into
`moatd.json`. See `moat template show <name>`.

Imported templates are linux/amd64 only and are not pinned in
`images.lock`.
```

- [ ] **Step 3: Update `docs/developer-guide.md`**

In §5 (the image pipeline), after the `build/templates/` subsection, add a short paragraph:

```markdown
### 5.8 Import an existing image — `moat template import`

`moat template import <ref>` (or `--from-archive <file>`) builds a
template rootfs straight from a container image: `podman pull` /
`podman load` → record the `@sha256` digest → read the image config →
`podman export` → inject `/sbin/guest-agent` → auto-size →
`mkfs.ext4 -d`. The pipeline is `internal/imgimport`. The image's
`ENV` / `WORKDIR` / `ENTRYPOINT` become the template's `env` /
`workdir` / `default_argv` (§7). linux/amd64 only. Not in
`images.lock` — the `source` digest is the record.
```

In §7 (using it), where templates and `moat template` are described, add:

```markdown
`moatd` applies a template's `env` and `workdir` to every `exec` in a
sandbox from that template, and runs `default_argv` when a request
sends no `argv`. A request `env` key, `dir`, or `argv` always wins.
`moat template show <name>` prints a template's exec defaults.
```

- [ ] **Step 4: Mirror the two additions into `docs/developer-guide.html`**

Match the file's existing markup (it uses `<h4>` inside a `<section>`, `<code>`, and `<p>`). Add:
- In the image-pipeline section, after the `build/templates/` block: an `<h4>Import an existing image — <code>moat template import</code></h4>` with a one-paragraph description mirroring Step 3.
- In the "Using it" section, near the `moat template` table: one `<p>` mirroring the Step 3 exec-defaults note.

Do not add new numbered `<h2>` sections — the guide's table of contents auto-numbers those.

- [ ] **Step 5: Add a line to `README.md`**

In the "Demo" / templates area, after the `build/templates/` reference:

```markdown
Import any linux/amd64 container as a template with
`moat template import python:3.12` — it carries the image's
`ENV` / `WORKDIR` / `ENTRYPOINT` forward as exec defaults.
```

- [ ] **Step 6: Verify the docs build clean**

Run:
```bash
jq -e . docs/moatd.example.json >/dev/null && echo json-ok
grep -n "template import" README.md docs/developer-guide.md build/templates/README.md
```
Expected: `json-ok` and the new references present.

- [ ] **Step 7: Commit**

```bash
git add docs/moatd.example.json build/templates/README.md docs/developer-guide.md docs/developer-guide.html README.md
git commit -m "docs: moat template import — guide, README, example config"
```

---

## Final verification (run before finishing the branch)

- [ ] `gofmt -l internal/ cmd/` prints nothing.
- [ ] `go vet ./...` is clean.
- [ ] `go build ./...` succeeds.
- [ ] `go test ./...` — all unit tests pass.
- [ ] `make agent && go test -tags integration ./internal/imgimport/ ./internal/server/` — passes on this box (skips only where `podman` / `/dev/kvm` / `debugfs` are absent).
- [ ] Manual smoke: `go build -o bin/moat ./cmd/moat && ./bin/moat template import --dry-run alpine:3.20` prints a sensible plan; then `make agent && ./bin/moat template import alpine:3.20 --name smoke --data /tmp/moat-smoke` builds `/tmp/moat-smoke/smoke-rootfs.ext4` and writes `/tmp/moat-smoke/smoke.template.json`.
- [ ] `git log --oneline main..HEAD` shows one commit per task, each message in the imperative.

Then use **superpowers:finishing-a-development-branch**.

---

## Self-Review

**Spec coverage:**
- `internal/imgimport` pipeline (spec §"Component detail / `internal/imgimport`", steps 1-8) → Tasks 1-3. Digest pin, arch check, config parse, export, agent inject, `/bin/sh` warning, normalise, auto-size, deterministic UUID/seed, `.tmp` rename, packages.manifest — all covered.
- `moat template import` CLI + flags (spec §"`cmd/moat`") → Task 7. Name derivation, `--from-archive`, `--size`, `--data`, `--agent`, `--config`, `--dry-run`, `--json`, sidecar + snippet, exit codes — covered.
- `moat template show` + `template list` scan (spec §"`cmd/moat`") → Task 8.
- `fileTemplate` + `server.Template` fields (spec §"`cmd/moatd/config.go`") → Tasks 4-5.
- `handleExec` merge semantics + `Sandbox.tpl` + audit `argv_source` + union `env_keys` + secret-scan-req-only + proxy vars on merged env (spec §"`internal/server`") → Tasks 4, 6.
- Error handling table (spec §"Error handling") → Task 1 (podman missing, pull fail, arch), Task 2 (mkfs too small, agent missing), Task 3 (arch re-check, no digest), Task 7 (dup name, bad `--name`, bad `--size`), `registerTemplate` (atomic write).
- Testing matrix (spec §"Testing") → unit tests in Tasks 1, 2, 4, 5, 7, 8; integration in Tasks 3, 6.
- Docs + non-goals (spec §"Docs to update", §"Non-goals") → Task 9. Non-goals need no code.
- **Gap check:** the spec's `SourceDateEpoch` option is on `imgimport.Options` (Task 3) and defaulted in `assembleRootfs` (Task 2); the CLI does not expose it (matches spec — no `--source-date-epoch` flag listed). Acceptable.

**Placeholder scan:** the only literal `REPLACE_WITH_REAL_DIGEST` is in Task 9 Step 1, an intentional example-config placeholder the implementer fills from a real `podman inspect`; every code step has complete code.

**Type consistency:**
- `Result` fields (`RootFSPath`, `Source`, `Env`, `Dir`, `DefaultArgv`, `SizeBytes`, `Warnings`, `PackagesManifest`) — defined Task 3, consumed Task 7 with matching names.
- `Template` fields (`Env`, `Dir`, `DefaultArgv`, `Source`) — defined Task 4, consumed Tasks 5 (`config.go` literal), 6 (`mergeExec` already in Task 4), and the Task 6 integration test.
- `templateEntry` json tags (`name`, `rootfs`, `env`, `workdir`, `default_argv`, `source`) — defined Task 7, match `fileTemplate` tags from Task 5, consumed Task 8.
- `mergeExec` signature `(argv []string, env map[string]string, dir, argvSource string, err error)` — defined Task 4, called identically in Task 6 Step 4.
- `deriveName` returns `(string, error)` in Task 7; Task 7 test and body agree.
- `assembleRootfs` signature is identical in Task 2 (definition) and Task 3 (call site).
