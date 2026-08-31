# Reproducible Image Builds Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a byte-identical `rootfs.ext4` on a fixed toolchain, pin the kernel to an exact hash-checked artifact, ship a CycloneDX SBOM and a checked-in `images.lock`, and make `moatd` verify the kernel and rootfs against the lock at startup, failing closed on a mismatch.

**Architecture:** The build scripts under `build/` become deterministic (pinned base image digest, `SOURCE_DATE_EPOCH`, fixed ext4 UUID and hash seed, `-trimpath` agent build). A new `go run ./build/sbomgen` reads the built rootfs's apk database and the guest-agent's module list and emits CycloneDX 1.5 JSON. `build/gen-images-lock.sh` records the three sha256 sums plus provenance into `images.lock`, a copy of which is checked in at the repo root. A new stdlib-only `internal/imagelock` package loads and verifies the lock. `moatd` runs the verification once at startup, drops any template whose images do not match, exits non-zero in `enforce` mode when no template survives, and records the digests in the `daemon.start` audit event.

**Tech Stack:** Go 1.27 (stdlib only — no new module), Bash, `podman`, `e2fsprogs` (`mkfs.ext4`, `debugfs`), optional `cosign`. CycloneDX 1.5 output is hand-marshaled Go structs.

**Spec:** `docs/superpowers/specs/2026-08-31-reproducible-image-builds-design.md`

## Global Constraints

- No new Go module dependency. CycloneDX output is hand-marshaled structs. `cosign` is an optional external binary, invoked only when configured.
- All comments and all log / error strings follow ASD-STE100: active voice, one instruction per sentence, ≤ 20 words for instructions and ≤ 25 for descriptions, no semicolons, no phrasal verbs ("start"/"stop", not "boot"/"tear down"; "remove", not "take off"), simple tenses, no nominalisation. Keep one verb for one action.
- The rootfs build stays rootless (Gotcha G-4): no loop mount. Read one file out of the ext4 with `debugfs -R`, never a mount.
- Go 1.27. The module declares `toolchain go1.27.0`.
- The reproducibility guarantee holds for a fixed toolchain (same `e2fsprogs`, `podman`, `go`, `tar`). Across versions the package set and the SBOM stay identical but the ext4 byte layout may shift. `moatd` enforces the digest from the lock that shipped with the image.
- `moatd` verification is on by default in `enforce` mode. A missing `images.lock` is treated as `warn` in every mode, so an existing install still starts.
- Every commit ends with these two trailers:
  ```
  Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01PRvf4bxSFzyL2dAPgorw7M
  ```

---

### Task 1: Pin the Go toolchain and make the guest-agent build deterministic

**Files:**
- Modify: `go.mod` (add a `toolchain` line)
- Modify: `Makefile:39-40` (the `agent:` target)
- Modify: `build/release.sh:20-21` (the agent build flags)

**Interfaces:**
- Consumes: nothing.
- Produces: a `bin/guest-agent` whose `go version -m` output shows `-trimpath=true` and no `vcs.*` stamps. Task 3 (rootfs) and Task 4 (sbomgen) read this binary.

- [ ] **Step 1: Add the toolchain directive**

Edit `go.mod`. After the `go 1.27` line, add:

```
toolchain go1.27.0
```

Leave everything else in `go.mod` unchanged.

- [ ] **Step 2: Verify the toolchain does not force a download**

The `Makefile` already exports `GOTOOLCHAIN ?= local` (line 12), so `go` uses the installed 1.27.0 and never downloads. Run:

```bash
go env GOTOOLCHAIN
go version
```

Expected: `GOTOOLCHAIN` prints `local` (or is empty, which also means "use the local toolchain when it satisfies `go.mod`"), and `go version` prints `go1.27.0`. If `go version` is not `go1.27.0`, stop and report — the build host must have exactly this version for reproducibility.

- [ ] **Step 3: Make the `agent:` target deterministic**

In `Makefile`, replace the `agent:` recipe (line 40):

```make
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags='-s -w' -o $(BIN)/guest-agent ./guest/agent
```

with:

```make
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -trimpath -buildvcs=false -ldflags='-s -w -buildid=' -o $(BIN)/guest-agent ./guest/agent
```

- [ ] **Step 4: Match the release build**

In `build/release.sh`, change line 21 from:

```sh
LDFLAGS="-s -w -X main.version=$VERSION"
```

to:

```sh
LDFLAGS="-s -w -buildid= -X main.version=$VERSION"
```

and change the `guest-agent` build line (line 28) from:

```sh
go build -ldflags "$LDFLAGS" -o "$STAGE/share/guest-agent" ./guest/agent
```

to:

```sh
go build -trimpath -buildvcs=false -ldflags "$LDFLAGS" -o "$STAGE/share/guest-agent" ./guest/agent
```

Leave the other four `go build` lines in `release.sh` unchanged — only the guest-agent goes into the rootfs, so only it must be reproducible.

- [ ] **Step 5: Verify the agent build is deterministic**

```bash
make agent && sha256sum bin/guest-agent
mv bin/guest-agent /tmp/agent.1
make agent && sha256sum bin/guest-agent
```

Expected: the two sha256 sums are identical. Also run:

```bash
go version -m bin/guest-agent | grep -E 'trimpath|vcs'
```

Expected: a line `build -trimpath=true` and no `build vcs.*` lines.

- [ ] **Step 6: Commit**

```bash
git add go.mod Makefile build/release.sh
git commit -m "$(cat <<'EOF'
build: pin the toolchain and make the guest-agent build reproducible

Add toolchain go1.27.0. Build the guest agent with -trimpath,
-buildvcs=false, and -buildid= so the same source gives the same
binary. The binary goes into the rootfs, so it must be reproducible.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01PRvf4bxSFzyL2dAPgorw7M
EOF
)"
```

---

### Task 2: Pin the kernel fetch

**Files:**
- Modify: `build/fetch-kernel.sh` (replace the dynamic snapshot/version resolution with pinned constants and a sha256 check)

**Interfaces:**
- Consumes: nothing.
- Produces: `build/artifacts/vmlinux` at a known sha256. Task 5 (`gen-images-lock.sh`) reads the four pinned values (`KERNEL_CI_SNAPSHOT`, `KERNEL_VERSION_EXACT`, `KERNEL_URL`, `KERNEL_SHA256`) by re-`source`-ing this script's variable block is NOT done — instead Task 5 passes them on the command line from Makefile variables. See Task 5.

- [ ] **Step 1: Resolve the pin values once**

Run these commands and record the output — you will paste the results into the script:

```bash
BUCKET="https://s3.amazonaws.com/spec.ccfc.min"
# newest snapshot dir:
curl -fsSL "${BUCKET}?list-type=2&prefix=firecracker-ci/&delimiter=/" \
  | grep -oE 'firecracker-ci/[0-9]{8}-[0-9a-f]+-0/' | sed 's#firecracker-ci/##; s#/##' | sort | tail -1
# with SNAP set to that value, newest 6.1.x:
SNAP=<paste snapshot>
curl -fsSL "${BUCKET}?list-type=2&prefix=firecracker-ci/${SNAP}/x86_64/vmlinux-" \
  | grep -oE "firecracker-ci/${SNAP}/x86_64/vmlinux-6\.1\.[0-9]+" | sort -V -u | tail -1
# download and hash it:
KEY=<paste that key>
curl -fsSL -o /tmp/vmlinux.pin "${BUCKET}/${KEY}"
sha256sum /tmp/vmlinux.pin
```

You now have: the snapshot (for example `20260826-761f88fbb951-0`), the exact version (the `6.1.NNN` from the key), the full URL (`${BUCKET}/${KEY}`), and the sha256.

- [ ] **Step 2: Rewrite the script header with the pins**

Replace `build/fetch-kernel.sh` lines 15-26 (from `set -euo pipefail` through the "already present" block) with:

```bash
set -euo pipefail

OUT="${1:-build/artifacts/vmlinux}"
ARCH="$(uname -m)"

# Pinned kernel. Bump these four values together, then run
# `make images && make images-verify` and commit the new images.lock.
KERNEL_CI_SNAPSHOT="${KERNEL_CI_SNAPSHOT:-<PASTE SNAPSHOT>}"
KERNEL_VERSION_EXACT="${KERNEL_VERSION_EXACT:-<PASTE 6.1.NNN>}"
KERNEL_SHA256="${KERNEL_SHA256:-<PASTE SHA256>}"

BUCKET="https://s3.amazonaws.com/spec.ccfc.min"
KEY="firecracker-ci/${KERNEL_CI_SNAPSHOT}/${ARCH}/vmlinux-${KERNEL_VERSION_EXACT}"
KERNEL_URL="${BUCKET}/${KEY}"

mkdir -p "$(dirname "$OUT")"

verify() {
  local f="$1"
  echo "${KERNEL_SHA256}  ${f}" | sha256sum -c - >/dev/null 2>&1
}

if [[ -s "$OUT" ]] && verify "$OUT"; then
  echo "kernel already present and verified: $OUT"
  exit 0
fi
```

- [ ] **Step 3: Replace the dynamic download body**

Replace `build/fetch-kernel.sh` lines 28-62 (the `snapshot=` resolution, the `key=` resolution, the fallback, and the download) with:

```bash
echo "downloading $KERNEL_URL"
curl -fSL --progress-bar -o "${OUT}.tmp" "$KERNEL_URL"

if ! verify "${OUT}.tmp"; then
  got="$(sha256sum "${OUT}.tmp" | cut -d' ' -f1)"
  rm -f "${OUT}.tmp"
  echo "ERROR: kernel sha256 mismatch" >&2
  echo "  want ${KERNEL_SHA256}" >&2
  echo "  got  ${got}" >&2
  exit 1
fi
mv "${OUT}.tmp" "$OUT"
echo "wrote $OUT (${KERNEL_VERSION_EXACT}, snapshot ${KERNEL_CI_SNAPSHOT})"

# Also download the matching config file for reference. Ignore errors.
curl -fsSL -o "${OUT}.config" "${KERNEL_URL}.config" 2>/dev/null || true
```

Keep the file's top comment block, but change the sentence "the newest dir under firecracker-ci/" wording to describe the pinned behaviour: "It downloads one pinned kernel and checks its sha256."

- [ ] **Step 4: Verify**

```bash
rm -f build/artifacts/vmlinux build/artifacts/vmlinux.config
build/fetch-kernel.sh build/artifacts/vmlinux
sha256sum build/artifacts/vmlinux            # must equal the pinned value
build/fetch-kernel.sh build/artifacts/vmlinux  # second run: "already present and verified", no download
printf 'bad' >> build/artifacts/vmlinux
build/fetch-kernel.sh build/artifacts/vmlinux ; echo "exit=$?"  # re-downloads because verify fails
```

Expected: first run downloads and the sha256 matches the pin. Second run prints "already present and verified" and exits 0 with no download. After corruption, the script re-downloads and restores the good file.

- [ ] **Step 5: Commit**

```bash
git add build/fetch-kernel.sh
git commit -m "$(cat <<'EOF'
build: pin the kernel to one snapshot and check its sha256

fetch-kernel.sh downloaded whatever the newest Firecracker CI snapshot
held, so two runs a month apart got different kernels. Pin the snapshot,
the exact 6.1.x version, and the sha256. Verify on every fetch.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01PRvf4bxSFzyL2dAPgorw7M
EOF
)"
```

---

### Task 3: Deterministic rootfs build

**Files:**
- Modify: `build/build-rootfs.sh` (pin base by digest, re-stamp the tree, fixed ext4 UUID and hash seed, `E2FSPROGS_FAKE_TIME`, freeze `ROOTFS_SIZE`)
- Modify: `Makefile:11,13,62-63` (stop passing `ROOTFS_SIZE` to the script)

**Interfaces:**
- Consumes: `bin/guest-agent` from Task 1.
- Produces: `build/artifacts/rootfs.ext4`, byte-identical across runs on one toolchain. Task 4 (`sbomgen`) reads its apk database. Task 5 records its sha256, base digest, fs UUID, and mkfs version.

- [ ] **Step 1: Resolve the base image digest**

```bash
podman pull docker.io/library/alpine:3.20
podman image inspect docker.io/library/alpine:3.20 --format '{{index .RepoDigests 0}}'
```

Record the `docker.io/library/alpine@sha256:...` string.

- [ ] **Step 2: Pick the fixed epoch**

```bash
date -u -d 2026-08-31 +%s
```

Record the integer (it is `1756598400`). This is `SOURCE_DATE_EPOCH`.

- [ ] **Step 3: Rewrite the script's constants block**

Replace `build/build-rootfs.sh` lines 20-23 with:

```bash
OUT="${1:-build/artifacts/rootfs.ext4}"
AGENT="${2:-bin/guest-agent}"

# Reproducibility pins. Bump BASE_IMAGE together with a fresh images.lock.
BASE_IMAGE="${BASE_IMAGE:-docker.io/library/alpine@sha256:<PASTE DIGEST>}"
ROOTFS_SIZE="256M"
SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-1756598400}"
FS_UUID="${FS_UUID:-5a5a5a5a-0000-4000-8000-000000000001}"
HASH_SEED="${HASH_SEED:-9b7a6c5d-1e2f-4a3b-8c9d-0e1f2a3b4c5d}"
export SOURCE_DATE_EPOCH
```

- [ ] **Step 4: Add the debugfs preflight**

After the existing `command -v mkfs.ext4 ...` check (line 26), add:

```bash
command -v debugfs >/dev/null || echo "WARN: debugfs not found — the SBOM step will fail" >&2
```

- [ ] **Step 5: Re-stamp the extracted tree**

After the `podman export ... | tar ... -xf -` line and the `podman rm` line (line 41), and after the agent `install` and the `mkdir -p` for mount points (line 47), but before the `mkfs.ext4` call, add:

```bash
echo "normalising timestamps to $SOURCE_DATE_EPOCH ..."
find "$root" -exec touch --no-dereference --date="@$SOURCE_DATE_EPOCH" {} +
```

- [ ] **Step 6: Make the mkfs call deterministic**

Replace the `mkfs.ext4` line (line 54):

```bash
mkfs.ext4 -q -F -L rootfs -d "$root" "$OUT" "$ROOTFS_SIZE"
```

with:

```bash
E2FSPROGS_FAKE_TIME="$SOURCE_DATE_EPOCH" mkfs.ext4 -q -F -L rootfs \
  -U "$FS_UUID" \
  -E "hash_seed=$HASH_SEED,lazy_itable_init=0,lazy_journal_init=0" \
  -d "$root" "$OUT" "$ROOTFS_SIZE"
```

- [ ] **Step 7: Stop the Makefile passing a size**

In `Makefile`:
- Delete line 11 (`ROOTFS_SIZE ?= 256M`) and line 13 (`export ROOTFS_SIZE`).
- Line 63 stays `build/build-rootfs.sh "$(ROOTFS)" "$(BIN)/guest-agent"` — it already passes only the two positional args.

- [ ] **Step 8: Verify byte-for-byte reproducibility**

```bash
make agent
build/build-rootfs.sh /tmp/rootfs.a bin/guest-agent
build/build-rootfs.sh /tmp/rootfs.b bin/guest-agent
sha256sum /tmp/rootfs.a /tmp/rootfs.b
```

Expected: the two sha256 sums are identical. If they differ, run `cmp -l /tmp/rootfs.a /tmp/rootfs.b | head` and inspect the first differing offset — the usual causes are a missed timestamp, a lazy-init flag, or a `podman` version difference.

- [ ] **Step 9: Commit**

```bash
git add build/build-rootfs.sh Makefile
git commit -m "$(cat <<'EOF'
build: make the rootfs.ext4 build byte-identical

Pin the base image by digest, normalise every file time to
SOURCE_DATE_EPOCH, and give mkfs.ext4 a fixed UUID, a fixed hash seed,
eager table init, and E2FSPROGS_FAKE_TIME. Freeze ROOTFS_SIZE at 256M.
Two builds on one toolchain now produce the same bytes.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01PRvf4bxSFzyL2dAPgorw7M
EOF
)"
```

---

### Task 4: `build/sbomgen` — the CycloneDX SBOM generator

**Files:**
- Create: `build/sbomgen/main.go`
- Create: `build/sbomgen/cyclonedx.go`
- Create: `build/sbomgen/parse.go`
- Create: `build/sbomgen/parse_test.go`
- Create: `build/sbomgen/cyclonedx_test.go`
- Create: `build/sbomgen/testdata/apk-installed.txt`
- Create: `build/sbomgen/testdata/go-version-m.txt`

**Interfaces:**
- Consumes: `build/artifacts/rootfs.ext4` (via `debugfs`), `bin/guest-agent` (via `go version -m`), and the pinned kernel/base values passed as flags.
- Produces: `build/artifacts/sbom.cdx.json`. Task 5 records its sha256. Run only with `go run ./build/sbomgen`; it is never a shipped binary.

- [ ] **Step 1: Create the apk fixture**

Create `build/sbomgen/testdata/apk-installed.txt` with two package records in real apk format (blank line between records):

```
C:Q1aBcDeFgH000000000000000000000000=
P:musl
V:1.2.5-r0
A:x86_64
L:MIT
c:abc123def4567890abc123def4567890abc12345

C:Q1zYxWvUtS111111111111111111111111=
P:busybox
V:1.36.1-r29
A:x86_64
L:GPL-2.0-only
c:def456abc7890123def456abc7890123def45678
```

- [ ] **Step 2: Create the `go version -m` fixture**

Create `build/sbomgen/testdata/go-version-m.txt`:

```
bin/guest-agent: go1.27.0
	path	github.com/M30W-M1X/moat/guest/agent
	mod	github.com/M30W-M1X/moat	(devel)
	dep	google.golang.org/grpc	v1.71.0	h1:xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx=
	dep	google.golang.org/protobuf	v1.36.5	h1:yyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyy=
	build	-trimpath=true
	build	CGO_ENABLED=0
```

- [ ] **Step 3: Write the failing parse test**

Create `build/sbomgen/parse_test.go`:

```go
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
```

- [ ] **Step 4: Run the test to see it fail**

Run: `go test ./build/sbomgen/ -run 'TestParse' -v`
Expected: FAIL — `parseAPKInstalled` and `parseGoModules` are undefined.

- [ ] **Step 5: Write the parsers**

Create `build/sbomgen/parse.go`:

```go
package main

import (
	"bufio"
	"bytes"
	"strings"
)

// apkPackage is one row from /lib/apk/db/installed.
type apkPackage struct {
	Name    string
	Version string
	Arch    string
	License string
	Commit  string
}

// goModule is one dependency from `go version -m`.
type goModule struct {
	Path    string
	Version string
}

// parseAPKInstalled reads an apk "installed" database. Each record is a block of
// "K:value" lines. A blank line ends a record.
func parseAPKInstalled(raw []byte) []apkPackage {
	var out []apkPackage
	var cur apkPackage
	has := false
	flush := func() {
		if has && cur.Name != "" {
			out = append(out, cur)
		}
		cur, has = apkPackage{}, false
	}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			flush()
			continue
		}
		if len(line) < 2 || line[1] != ':' {
			continue
		}
		key, val := line[0], line[2:]
		has = true
		switch key {
		case 'P':
			cur.Name = val
		case 'V':
			cur.Version = val
		case 'A':
			cur.Arch = val
		case 'L':
			cur.License = val
		case 'c':
			cur.Commit = val
		}
	}
	flush()
	return out
}

// parseGoModules reads `go version -m <bin>` output and returns the "dep" rows.
// It skips the "mod" row (the main module) and the "build" rows.
func parseGoModules(raw []byte) []goModule {
	var out []goModule
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 3 && fields[0] == "dep" {
			out = append(out, goModule{Path: fields[1], Version: fields[2]})
		}
	}
	return out
}
```

- [ ] **Step 6: Run the parse test to see it pass**

Run: `go test ./build/sbomgen/ -run 'TestParse' -v`
Expected: PASS.

- [ ] **Step 7: Write the failing CycloneDX test**

Create `build/sbomgen/cyclonedx_test.go`:

```go
package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestBuildBOMDeterministic(t *testing.T) {
	in := bomInput{
		SourceDateEpoch: 1756598400,
		RootfsSHA256:    "aaaa",
		BaseImage:       "docker.io/library/alpine@sha256:bbbb",
		KernelVersion:   "6.1.150",
		KernelSnapshot:  "20260826-abc-0",
		KernelSHA256:    "cccc",
		APK: []apkPackage{
			{Name: "musl", Version: "1.2.5-r0", Arch: "x86_64", License: "MIT"},
		},
		GoModules: []goModule{
			{Path: "google.golang.org/grpc", Version: "v1.71.0"},
		},
	}
	a, err := buildBOM(in)
	if err != nil {
		t.Fatal(err)
	}
	b, err := buildBOM(in)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("buildBOM is not deterministic for the same input")
	}

	var doc map[string]any
	if err := json.Unmarshal(a, &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if doc["bomFormat"] != "CycloneDX" || doc["specVersion"] != "1.5" {
		t.Fatalf("wrong header: %v %v", doc["bomFormat"], doc["specVersion"])
	}
	comps, _ := doc["components"].([]any)
	// alpine OS + 1 apk lib + guest-agent app + 1 go lib + kernel data = 5
	if len(comps) != 5 {
		t.Fatalf("got %d components, want 5", len(comps))
	}
	// The timestamp comes only from SourceDateEpoch.
	meta, _ := doc["metadata"].(map[string]any)
	if meta["timestamp"] != "2026-08-31T00:00:00Z" {
		t.Fatalf("timestamp = %v, want 2026-08-31T00:00:00Z", meta["timestamp"])
	}
}
```

- [ ] **Step 8: Run it to see it fail**

Run: `go test ./build/sbomgen/ -run 'TestBuildBOM' -v`
Expected: FAIL — `bomInput` and `buildBOM` are undefined.

- [ ] **Step 9: Write the CycloneDX builder**

Create `build/sbomgen/cyclonedx.go`:

```go
package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// bomInput carries everything buildBOM needs. The caller fills it from the
// rootfs, the agent binary, and the pinned kernel and base-image values.
type bomInput struct {
	SourceDateEpoch int64
	RootfsSHA256    string
	BaseImage       string // docker.io/library/alpine@sha256:...
	KernelVersion   string
	KernelSnapshot  string
	KernelSHA256    string
	APK             []apkPackage
	GoModules       []goModule
}

type cdxHash struct {
	Alg     string `json:"alg"`
	Content string `json:"content"`
}

type cdxLicense struct {
	License struct {
		ID string `json:"id"`
	} `json:"license"`
}

type cdxProp struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type cdxComponent struct {
	Type       string       `json:"type"`
	Name       string       `json:"name"`
	Version    string       `json:"version,omitempty"`
	PURL       string       `json:"purl,omitempty"`
	Licenses   []cdxLicense `json:"licenses,omitempty"`
	Hashes     []cdxHash    `json:"hashes,omitempty"`
	Properties []cdxProp    `json:"properties,omitempty"`
}

type cdxDoc struct {
	BOMFormat   string `json:"bomFormat"`
	SpecVersion string `json:"specVersion"`
	Version     int    `json:"version"`
	Metadata    struct {
		Timestamp string       `json:"timestamp"`
		Component cdxComponent `json:"component"`
	} `json:"metadata"`
	Components []cdxComponent `json:"components"`
}

func lic(id string) []cdxLicense {
	if id == "" {
		return nil
	}
	var l cdxLicense
	l.License.ID = id
	return []cdxLicense{l}
}

// buildBOM renders a CycloneDX 1.5 document. The output is byte-identical for
// the same input, so the build can hash it into images.lock.
func buildBOM(in bomInput) ([]byte, error) {
	ts := time.Unix(in.SourceDateEpoch, 0).UTC().Format(time.RFC3339)

	var comps []cdxComponent

	comps = append(comps, cdxComponent{
		Type: "operating-system",
		Name: "alpine",
		PURL: "pkg:oci/alpine@" + digestOf(in.BaseImage),
	})

	for _, p := range in.APK {
		comps = append(comps, cdxComponent{
			Type:     "library",
			Name:     p.Name,
			Version:  p.Version,
			PURL:     fmt.Sprintf("pkg:apk/alpine/%s@%s?arch=%s", p.Name, p.Version, p.Arch),
			Licenses: lic(p.License),
		})
	}

	comps = append(comps, cdxComponent{
		Type: "application",
		Name: "guest-agent",
		PURL: "pkg:golang/github.com/M30W-M1X/moat/guest/agent",
	})
	for _, m := range in.GoModules {
		comps = append(comps, cdxComponent{
			Type:    "library",
			Name:    m.Path,
			Version: m.Version,
			PURL:    fmt.Sprintf("pkg:golang/%s@%s", m.Path, m.Version),
		})
	}

	comps = append(comps, cdxComponent{
		Type:    "data",
		Name:    "vmlinux",
		Version: in.KernelVersion,
		Hashes:  []cdxHash{{Alg: "SHA-256", Content: in.KernelSHA256}},
		Properties: []cdxProp{
			{Name: "moat:ci-snapshot", Value: in.KernelSnapshot},
		},
	})

	sort.Slice(comps, func(i, j int) bool {
		if comps[i].PURL != comps[j].PURL {
			return comps[i].PURL < comps[j].PURL
		}
		return comps[i].Name < comps[j].Name
	})

	var doc cdxDoc
	doc.BOMFormat = "CycloneDX"
	doc.SpecVersion = "1.5"
	doc.Version = 1
	doc.Metadata.Timestamp = ts
	doc.Metadata.Component = cdxComponent{
		Type:   "container",
		Name:   "moat-rootfs",
		Hashes: []cdxHash{{Alg: "SHA-256", Content: in.RootfsSHA256}},
	}
	doc.Components = comps

	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// digestOf returns the "sha256:..." part of a "name@sha256:..." reference, or
// the whole string when there is no "@".
func digestOf(ref string) string {
	for i := len(ref) - 1; i >= 0; i-- {
		if ref[i] == '@' {
			return ref[i+1:]
		}
	}
	return ref
}
```

- [ ] **Step 10: Run the CycloneDX test to see it pass**

Run: `go test ./build/sbomgen/ -run 'TestBuildBOM' -v`
Expected: PASS.

- [ ] **Step 11: Write `main.go`**

Create `build/sbomgen/main.go`:

```go
// Command sbomgen writes a CycloneDX 1.5 SBOM for the built rootfs. Run it with
// `go run ./build/sbomgen`. It is never a shipped binary.
//
//	go run ./build/sbomgen \
//	  -rootfs build/artifacts/rootfs.ext4 -agent bin/guest-agent \
//	  -kernel-version 6.1.150 -kernel-snapshot 20260826-abc-0 \
//	  -kernel-sha256 <hex> -base-image docker.io/library/alpine@sha256:<hex> \
//	  -source-date-epoch 1756598400 -out build/artifacts/sbom.cdx.json
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
)

func main() {
	rootfs := flag.String("rootfs", "build/artifacts/rootfs.ext4", "built rootfs image")
	agent := flag.String("agent", "bin/guest-agent", "guest-agent binary")
	kver := flag.String("kernel-version", "", "pinned kernel version, for example 6.1.150")
	ksnap := flag.String("kernel-snapshot", "", "pinned kernel CI snapshot")
	ksha := flag.String("kernel-sha256", "", "pinned kernel sha256")
	base := flag.String("base-image", "", "base image with digest")
	sde := flag.Int64("source-date-epoch", 0, "fixed build epoch")
	out := flag.String("out", "build/artifacts/sbom.cdx.json", "output path")
	flag.Parse()

	if *kver == "" || *ksnap == "" || *ksha == "" || *base == "" || *sde == 0 {
		fmt.Fprintln(os.Stderr, "sbomgen: -kernel-version, -kernel-snapshot, -kernel-sha256, -base-image, and -source-date-epoch are required")
		os.Exit(2)
	}

	rootfsSum, err := sha256File(*rootfs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sbomgen: hash rootfs:", err)
		os.Exit(1)
	}

	apkRaw, err := exec.Command("debugfs", "-R", "cat /lib/apk/db/installed", *rootfs).Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "sbomgen: read apk db with debugfs:", err)
		os.Exit(1)
	}
	modRaw, err := exec.Command("go", "version", "-m", *agent).Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "sbomgen: go version -m:", err)
		os.Exit(1)
	}

	apk := parseAPKInstalled(apkRaw)
	mods := parseGoModules(modRaw)

	doc, err := buildBOM(bomInput{
		SourceDateEpoch: *sde,
		RootfsSHA256:    rootfsSum,
		BaseImage:       *base,
		KernelVersion:   *kver,
		KernelSnapshot:  *ksnap,
		KernelSHA256:    *ksha,
		APK:             apk,
		GoModules:       mods,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "sbomgen: build:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, doc, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "sbomgen: write:", err)
		os.Exit(1)
	}
	// components = alpine OS + apk libs + guest-agent app + go libs + kernel.
	fmt.Printf("wrote %s (%d components)\n", *out, len(apk)+len(mods)+3)
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
```

- [ ] **Step 12: Verify the whole package builds, vets, and tests**

```bash
go build ./build/sbomgen/
go vet ./build/sbomgen/
go test ./build/sbomgen/ -v
gofmt -l build/sbomgen/
```

Expected: build and vet clean, all tests pass, `gofmt -l` prints nothing.

- [ ] **Step 13: Commit**

```bash
git add build/sbomgen/
git commit -m "$(cat <<'EOF'
build: add sbomgen, a dependency-free CycloneDX SBOM generator

sbomgen reads the rootfs apk database with debugfs, the guest-agent
module list with `go version -m`, and the pinned kernel and base-image
values. It emits deterministic CycloneDX 1.5 JSON. Run it with
`go run ./build/sbomgen`; it is never a shipped binary.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01PRvf4bxSFzyL2dAPgorw7M
EOF
)"
```

---

### Task 5: Pipeline wiring — `gen-images-lock.sh`, Makefile targets, checked-in `images.lock`, `images-verify`

**Files:**
- Create: `build/gen-images-lock.sh`
- Create: `images.lock` (repo root, checked in — generated by running the pipeline)
- Modify: `Makefile` (add `sbom`, `lock`, `images-verify`; change `images:` deps)
- Modify: `.gitignore` (confirm `build/artifacts/` stays ignored; `images.lock` at the repo root is NOT under it, so no change unless a rule catches it — verify)

**Interfaces:**
- Consumes: `build/artifacts/vmlinux`, `build/artifacts/rootfs.ext4`, `build/artifacts/sbom.cdx.json`, `bin/guest-agent`, and the pins from Tasks 2 and 3.
- Produces: `build/artifacts/images.lock` and the checked-in repo-root `images.lock`. Task 6 (`internal/imagelock`) loads this exact schema. Task 7 verifies against it.

- [ ] **Step 1: Add Makefile variables for the pins**

In `Makefile`, after the `ROOTFS := $(ARTIFACTS)/rootfs.ext4` line (was line 16, now line 14 after Task 3 removed two lines), add:

```make
SBOM    := $(ARTIFACTS)/sbom.cdx.json
LOCK    := $(ARTIFACTS)/images.lock

# Pins — keep in sync with build/fetch-kernel.sh and build/build-rootfs.sh.
KERNEL_VERSION_EXACT ?= <PASTE 6.1.NNN>
KERNEL_CI_SNAPSHOT   ?= <PASTE SNAPSHOT>
KERNEL_SHA256        ?= <PASTE KERNEL SHA256>
BASE_IMAGE           ?= docker.io/library/alpine@sha256:<PASTE DIGEST>
SOURCE_DATE_EPOCH    ?= 1756598400
export SOURCE_DATE_EPOCH
```

- [ ] **Step 2: Rewrite the `images` target and add `sbom` / `lock`**

Replace the `images: kernel rootfs` line (was line 53) with:

```make
images: kernel rootfs sbom lock

## generate the CycloneDX SBOM for the built rootfs
sbom: $(SBOM)
$(SBOM): $(ROOTFS) agent
	$(GO) run ./build/sbomgen \
	  -rootfs "$(ROOTFS)" -agent "$(BIN)/guest-agent" \
	  -kernel-version "$(KERNEL_VERSION_EXACT)" -kernel-snapshot "$(KERNEL_CI_SNAPSHOT)" \
	  -kernel-sha256 "$(KERNEL_SHA256)" -base-image "$(BASE_IMAGE)" \
	  -source-date-epoch "$(SOURCE_DATE_EPOCH)" -out "$(SBOM)"

## write build/artifacts/images.lock from the built images and the SBOM
lock: $(LOCK)
$(LOCK): $(KERNEL) $(ROOTFS) $(SBOM) $(BIN)/guest-agent
	build/gen-images-lock.sh "$(ARTIFACTS)" \
	  "$(KERNEL_VERSION_EXACT)" "$(KERNEL_CI_SNAPSHOT)" \
	  "$(BASE_IMAGE)" "$(SOURCE_DATE_EPOCH)" > "$(LOCK)"

## build the rootfs twice and assert the bytes match images.lock
images-verify: images
	build/images-verify.sh "$(ARTIFACTS)" "$(BIN)/guest-agent" \
	  "$(KERNEL_VERSION_EXACT)" "$(KERNEL_CI_SNAPSHOT)" \
	  "$(KERNEL_SHA256)" "$(BASE_IMAGE)" "$(SOURCE_DATE_EPOCH)"
```

Add `sbom lock images-verify` to the `.PHONY` line (was line 18).

- [ ] **Step 3: Write `build/gen-images-lock.sh`**

Create `build/gen-images-lock.sh` (mode 0755):

```bash
#!/usr/bin/env bash
# Write images.lock to stdout from the built artifacts.
#
# usage: build/gen-images-lock.sh ARTIFACTS_DIR KERNEL_VERSION KERNEL_SNAPSHOT BASE_IMAGE SOURCE_DATE_EPOCH
set -euo pipefail

ART="$1"; KVER="$2"; KSNAP="$3"; BASE="$4"; SDE="$5"
ARCH="$(uname -m)"
BUCKET="https://s3.amazonaws.com/spec.ccfc.min"
KURL="${BUCKET}/firecracker-ci/${KSNAP}/${ARCH}/vmlinux-${KVER}"

sha() { sha256sum "$1" | cut -d' ' -f1; }

KSHA="$(sha "$ART/vmlinux")"
RSHA="$(sha "$ART/rootfs.ext4")"
SSHA="$(sha "$ART/sbom.cdx.json")"
ASHA="$(sha "bin/guest-agent")"
RSIZE="$(stat -c %s "$ART/rootfs.ext4")"
MKFSV="$(mkfs.ext4 -V 2>&1 | head -1)"
GOV="$(go version | awk '{print $3}')"
FS_UUID="${FS_UUID:-5a5a5a5a-0000-4000-8000-000000000001}"

cat <<JSON
{
  "schema": "moat.images.lock/v1",
  "source_date_epoch": ${SDE},
  "kernel": {
    "path": "vmlinux",
    "sha256": "${KSHA}",
    "ci_snapshot": "${KSNAP}",
    "version": "${KVER}",
    "source": "${KURL}"
  },
  "rootfs": {
    "path": "rootfs.ext4",
    "sha256": "${RSHA}",
    "size_bytes": ${RSIZE},
    "base_image": "${BASE}",
    "fs_uuid": "${FS_UUID}",
    "mkfs_version": "${MKFSV}"
  },
  "sbom": { "path": "sbom.cdx.json", "sha256": "${SSHA}" },
  "guest_agent": { "sha256": "${ASHA}", "go": "${GOV}" },
  "generated_by": "moat build pipeline",
  "generated_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
}
JSON
```

- [ ] **Step 4: Write `build/images-verify.sh`**

Create `build/images-verify.sh` (mode 0755):

```bash
#!/usr/bin/env bash
# Rebuild the rootfs and the SBOM, then assert byte-for-byte reproducibility
# and agreement with images.lock and the checked-in repo-root images.lock.
#
# usage: build/images-verify.sh ARTIFACTS AGENT KVER KSNAP KSHA BASE SDE
set -euo pipefail

ART="$1"; AGENT="$2"; KVER="$3"; KSNAP="$4"; KSHA="$5"; BASE="$6"; SDE="$7"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

sha() { sha256sum "$1" | cut -d' ' -f1; }
fail() { echo "FAIL: $1" >&2; exit 1; }

echo "rebuilding rootfs ..."
build/build-rootfs.sh "$tmp/rootfs.ext4" "$AGENT" >/dev/null

[ "$(sha "$tmp/rootfs.ext4")" = "$(sha "$ART/rootfs.ext4")" ] \
  || fail "rootfs.ext4 is not reproducible on this toolchain"

echo "rebuilding sbom ..."
go run ./build/sbomgen -rootfs "$tmp/rootfs.ext4" -agent "$AGENT" \
  -kernel-version "$KVER" -kernel-snapshot "$KSNAP" -kernel-sha256 "$KSHA" \
  -base-image "$BASE" -source-date-epoch "$SDE" -out "$tmp/sbom.cdx.json" >/dev/null

[ "$(sha "$tmp/sbom.cdx.json")" = "$(sha "$ART/sbom.cdx.json")" ] \
  || fail "sbom.cdx.json is not reproducible"

# images.lock, minus the one non-reproducible field, must match the repo-root copy.
strip() { grep -v '"generated_at"' "$1"; }
diff <(strip "$ART/images.lock") <(strip images.lock) \
  || fail "build/artifacts/images.lock differs from the repo-root images.lock — commit the new one"

# The lock's own digests must match the freshly built files.
grep -q "\"$(sha "$ART/rootfs.ext4")\"" "$ART/images.lock" || fail "images.lock rootfs sha256 is stale"
grep -q "\"$(sha "$ART/vmlinux")\"" "$ART/images.lock"      || fail "images.lock kernel sha256 is stale"

echo "reproducible: OK"
```

Add `build/images-verify.sh` to the `git add` in Step 8.

- [ ] **Step 5: Generate the real `images.lock`**

```bash
rm -rf build/artifacts
make images
cp build/artifacts/images.lock images.lock
cat images.lock
```

Expected: `make images` runs kernel → rootfs → sbom → lock with no error. `images.lock` has the `moat.images.lock/v1` schema and real sha256 values.

- [ ] **Step 6: Confirm the repo-root `images.lock` is not gitignored**

```bash
git check-ignore -v images.lock ; echo "exit=$?"
```

Expected: `exit=1` (not ignored). If it is ignored, add `!/images.lock` to `.gitignore` after the rule that catches it, and re-check.

- [ ] **Step 7: Run `make images-verify`**

```bash
make images-verify
```

Expected: it rebuilds the rootfs and the SBOM, prints `reproducible: OK`, and exits 0. If it fails on "not reproducible", go back to Task 3 Step 8 and find the drifting byte.

- [ ] **Step 8: Commit**

```bash
git add build/gen-images-lock.sh build/images-verify.sh Makefile images.lock .gitignore
git commit -m "$(cat <<'EOF'
build: images.lock, SBOM wiring, and make images-verify

`make images` now runs kernel -> rootfs -> sbom -> lock. gen-images-lock.sh
records the three sha256 sums plus provenance into images.lock, a copy of
which is checked in at the repo root. `make images-verify` rebuilds the
rootfs and the SBOM and asserts byte-for-byte reproducibility and
agreement with the checked-in lock.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01PRvf4bxSFzyL2dAPgorw7M
EOF
)"
```

---

### Task 6: `internal/imagelock` package

**Files:**
- Create: `internal/imagelock/imagelock.go`
- Create: `internal/imagelock/imagelock_test.go`

**Interfaces:**
- Consumes: the `images.lock` schema from Task 5.
- Produces:
  - `func Load(path string) (*Lock, error)`
  - `func Find(rootfsPath string) string`
  - `func (l *Lock) VerifyFiles(kernelPath, rootfsPath string) error`
  - `func (l *Lock) VerifySignature(lockPath, sigPath, identity, issuer string) error`
  - `type MismatchError struct{ File, Want, Got string }` with `Error() string`
  - `type Lock struct{ ... }` with exported fields `Schema string`, `SourceDateEpoch int64`, `Kernel FileEntry`, `Rootfs RootfsEntry`, `SBOM FileRef`, `GuestAgent AgentEntry`, `GeneratedBy string`, `GeneratedAt string`
  - `FileEntry{ Path, SHA256, CISnapshot, Version, Source string }`, `RootfsEntry{ Path, SHA256 string; SizeBytes int64; BaseImage, FSUUID, MkfsVersion string }`, `FileRef{ Path, SHA256 string }`, `AgentEntry{ SHA256, Go string }`
  Task 7 uses all of these.

- [ ] **Step 1: Write the failing tests**

Create `internal/imagelock/imagelock_test.go`:

```go
package imagelock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const sample = `{
  "schema": "moat.images.lock/v1",
  "source_date_epoch": 1756598400,
  "kernel": {"path":"vmlinux","sha256":"KSUM","ci_snapshot":"snap","version":"6.1.150","source":"http://x"},
  "rootfs": {"path":"rootfs.ext4","sha256":"RSUM","size_bytes":268435456,"base_image":"alpine@sha256:x","fs_uuid":"u","mkfs_version":"mke2fs 1.47.1"},
  "sbom": {"path":"sbom.cdx.json","sha256":"SSUM"},
  "guest_agent": {"sha256":"ASUM","go":"go1.27.0"},
  "generated_by": "moat build pipeline",
  "generated_at": "2026-08-31T00:00:00Z"
}`

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "images.lock")
	writeFile(t, p, sample)
	l, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if l.Schema != "moat.images.lock/v1" || l.Kernel.SHA256 != "KSUM" ||
		l.Rootfs.SHA256 != "RSUM" || l.SBOM.SHA256 != "SSUM" || l.GuestAgent.Go != "go1.27.0" {
		t.Fatalf("bad parse: %+v", l)
	}
}

func TestLoadRejectsUnknownSchema(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "images.lock")
	writeFile(t, p, `{"schema":"moat.images.lock/v2"}`)
	if _, err := Load(p); err == nil {
		t.Fatal("want an error for an unknown schema")
	}
}

func TestLoadRejectsMalformed(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "images.lock")
	writeFile(t, p, `{not json`)
	if _, err := Load(p); err == nil {
		t.Fatal("want an error for malformed JSON")
	}
}

func TestFind(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "images")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	rootfs := filepath.Join(sub, "rootfs.ext4")
	writeFile(t, rootfs, "x")

	if got := Find(rootfs); got != "" {
		t.Fatalf("no lock anywhere, got %q", got)
	}
	parentLock := filepath.Join(dir, "images.lock")
	writeFile(t, parentLock, sample)
	if got := Find(rootfs); got != parentLock {
		t.Fatalf("parent lock: got %q want %q", got, parentLock)
	}
	besideLock := filepath.Join(sub, "images.lock")
	writeFile(t, besideLock, sample)
	if got := Find(rootfs); got != besideLock {
		t.Fatalf("beside lock should win: got %q want %q", got, besideLock)
	}
}

func TestVerifyFiles(t *testing.T) {
	dir := t.TempDir()
	kern := filepath.Join(dir, "vmlinux")
	root := filepath.Join(dir, "rootfs.ext4")
	writeFile(t, kern, "kernel-bytes")
	writeFile(t, root, "rootfs-bytes")

	l := &Lock{}
	l.Kernel.SHA256 = sha256OfString("kernel-bytes")
	l.Rootfs.SHA256 = sha256OfString("rootfs-bytes")
	if err := l.VerifyFiles(kern, root); err != nil {
		t.Fatalf("want pass, got %v", err)
	}

	writeFile(t, root, "rootfs-bytes-TAMPERED")
	err := l.VerifyFiles(kern, root)
	var me *MismatchError
	if !errors.As(err, &me) {
		t.Fatalf("want *MismatchError, got %v", err)
	}
	if me.File != "rootfs.ext4" || me.Want == "" || me.Got == "" || me.Want == me.Got {
		t.Fatalf("mismatch fields wrong: %+v", me)
	}
}

func TestVerifySignatureNoop(t *testing.T) {
	l := &Lock{}
	// cosign absent + identity empty -> nil.
	if err := l.VerifySignature("images.lock", "images.lock.sig", "", ""); err != nil {
		t.Fatalf("want nil, got %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/imagelock/ -v`
Expected: FAIL — the package does not compile (`Load`, `Find`, `Lock`, `MismatchError`, `sha256OfString`, `VerifyFiles`, `VerifySignature` undefined).

- [ ] **Step 3: Write the package**

Create `internal/imagelock/imagelock.go`:

```go
// Package imagelock loads and verifies images.lock, the record of the sha256
// digests and provenance for a Moat kernel and rootfs pair.
package imagelock

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// Schema is the only images.lock version this build understands.
const Schema = "moat.images.lock/v1"

// FileEntry describes the pinned kernel.
type FileEntry struct {
	Path       string `json:"path"`
	SHA256     string `json:"sha256"`
	CISnapshot string `json:"ci_snapshot"`
	Version    string `json:"version"`
	Source     string `json:"source"`
}

// RootfsEntry describes the built rootfs.
type RootfsEntry struct {
	Path        string `json:"path"`
	SHA256      string `json:"sha256"`
	SizeBytes   int64  `json:"size_bytes"`
	BaseImage   string `json:"base_image"`
	FSUUID      string `json:"fs_uuid"`
	MkfsVersion string `json:"mkfs_version"`
}

// FileRef is a path and its digest.
type FileRef struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// AgentEntry describes the guest agent that went into the rootfs.
type AgentEntry struct {
	SHA256 string `json:"sha256"`
	Go     string `json:"go"`
}

// Lock is the parsed images.lock.
type Lock struct {
	SchemaField     string      `json:"schema"`
	SourceDateEpoch int64       `json:"source_date_epoch"`
	Kernel          FileEntry   `json:"kernel"`
	Rootfs          RootfsEntry `json:"rootfs"`
	SBOM            FileRef     `json:"sbom"`
	GuestAgent      AgentEntry  `json:"guest_agent"`
	GeneratedBy     string      `json:"generated_by"`
	GeneratedAt     string      `json:"generated_at"`
}

// MismatchError reports that a file's digest does not match the lock.
type MismatchError struct {
	File string
	Want string
	Got  string
}

func (e *MismatchError) Error() string {
	return fmt.Sprintf("image %s digest mismatch: want %s, got %s", e.File, e.Want, e.Got)
}

// Load reads and validates a lock file. It rejects an unknown schema.
func Load(path string) (*Lock, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var l Lock
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if l.SchemaField != Schema {
		return nil, fmt.Errorf("%s: unknown schema %q, want %q", path, l.SchemaField, Schema)
	}
	return &l, nil
}

// Find returns the images.lock path for a rootfs, or "" when none exists. It
// looks in the rootfs directory first, then the parent directory.
func Find(rootfsPath string) string {
	dir := filepath.Dir(rootfsPath)
	beside := filepath.Join(dir, "images.lock")
	if fileExists(beside) {
		return beside
	}
	parent := filepath.Join(filepath.Dir(dir), "images.lock")
	if fileExists(parent) {
		return parent
	}
	return ""
}

// VerifyFiles hashes kernelPath and rootfsPath and compares them to the lock. A
// mismatch returns a *MismatchError that names the file and both digests.
func (l *Lock) VerifyFiles(kernelPath, rootfsPath string) error {
	for _, c := range []struct {
		name, path, want string
	}{
		{l.Kernel.Path, kernelPath, l.Kernel.SHA256},
		{l.Rootfs.Path, rootfsPath, l.Rootfs.SHA256},
	} {
		got, err := sha256File(c.path)
		if err != nil {
			return fmt.Errorf("hash %s: %w", c.name, err)
		}
		if got != c.want {
			return &MismatchError{File: c.name, Want: c.want, Got: got}
		}
	}
	return nil
}

// VerifySignature runs `cosign verify-blob` on the lock. It returns nil when
// identity is empty and cosign is not on PATH. It returns an error when
// identity is set and the check fails.
func (l *Lock) VerifySignature(lockPath, sigPath, identity, issuer string) error {
	if identity == "" {
		return nil
	}
	if _, err := exec.LookPath("cosign"); err != nil {
		return fmt.Errorf("cosign identity is set but cosign is not on PATH")
	}
	if !fileExists(sigPath) {
		return fmt.Errorf("cosign identity is set but %s is missing", sigPath)
	}
	args := []string{
		"verify-blob",
		"--signature", sigPath,
		"--certificate-identity", identity,
	}
	if issuer != "" {
		args = append(args, "--certificate-oidc-issuer", issuer)
	}
	args = append(args, lockPath)
	out, err := exec.Command("cosign", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("cosign verify-blob: %v: %s", err, out)
	}
	return nil
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
```

Then add the test-only helper at the top of `internal/imagelock/imagelock_test.go`, after the imports:

```go
// sha256OfString builds a want value the same way VerifyFiles computes it.
func sha256OfString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
```

Add `"crypto/sha256"` and `"encoding/hex"` to the test file's imports.

Note: the struct tag for the schema field is `json:"schema"` but the Go field is `SchemaField` to avoid clashing with the `Schema` constant. The `Interfaces` block above lists `Schema string` for the reader's convenience — the real accessor is the `SchemaField` field. Task 7 reads `l.Rootfs.SHA256` and friends, not the schema.

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test ./internal/imagelock/ -v`
Expected: PASS for all six tests.

- [ ] **Step 5: Vet and format**

```bash
go vet ./internal/imagelock/
gofmt -l internal/imagelock/
```

Expected: clean, and `gofmt -l` prints nothing.

- [ ] **Step 6: Commit**

```bash
git add internal/imagelock/
git commit -m "$(cat <<'EOF'
feat(imagelock): load and verify images.lock

Load parses and schema-checks images.lock. Find locates the lock beside
a rootfs or in the parent directory. VerifyFiles streams sha256 over the
kernel and rootfs and returns a typed MismatchError. VerifySignature
runs cosign verify-blob when an identity is configured.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01PRvf4bxSFzyL2dAPgorw7M
EOF
)"
```

---

### Task 7: `moatd` startup verification gate

**Files:**
- Create: `internal/server/images_verify.go`
- Modify: `internal/server/server.go:36-70` (Config fields), `internal/server/server.go:139` area (`New`), `internal/server/server.go` `Start` (`daemon.start` attrs)
- Modify: `internal/server/server_integration_test.go` (three new tests + a small helper)

**Interfaces:**
- Consumes: `imagelock.Load`, `imagelock.Find`, `(*Lock).VerifyFiles`, `(*Lock).VerifySignature`, `*imagelock.MismatchError` from Task 6.
- Produces:
  - `Config` fields `ImageVerify string`, `CosignIdentity string`, `CosignIssuer string`.
  - `type imageResult struct{ Template, KernelSHA256, RootFSSHA256, SBOMSHA256 string; Verified, Signed bool }`
  - `func verifyImages(cfg Config, log *logrus.Logger) ([]imageResult, error)` — Task 8 does not call this directly, but `cmd/moatd` validation mirrors the allowed `ImageVerify` values.
  - The `daemon.start` audit event gains an `"images"` attribute.

- [ ] **Step 1: Write the failing tests**

In `internal/server/server_integration_test.go`, add near the other audit tests:

```go
// writeImagesLock writes a minimal valid images.lock next to rootfsPath whose
// digests match the real files. kernelPath and rootfsPath must exist.
func writeImagesLock(t *testing.T, kernelPath, rootfsPath string) string {
	t.Helper()
	ksum := sha256OfFile(t, kernelPath)
	rsum := sha256OfFile(t, rootfsPath)
	body := `{
  "schema": "moat.images.lock/v1",
  "source_date_epoch": 1756598400,
  "kernel": {"path":"vmlinux","sha256":"` + ksum + `","ci_snapshot":"s","version":"6.1.150","source":"http://x"},
  "rootfs": {"path":"rootfs.ext4","sha256":"` + rsum + `","size_bytes":1,"base_image":"alpine@sha256:x","fs_uuid":"u","mkfs_version":"m"},
  "sbom": {"path":"sbom.cdx.json","sha256":"deadbeef"},
  "guest_agent": {"sha256":"a","go":"go1.27.0"},
  "generated_by":"t","generated_at":"2026-08-31T00:00:00Z"
}`
	// The lock must sit beside the rootfs OR in its parent. The real images
	// live in build/artifacts, which the test must not write to. Use a temp
	// copy dir instead.
	p := filepath.Join(filepath.Dir(rootfsPath), "images.lock")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(p) })
	return p
}

func sha256OfFile(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// copyImagesToTemp copies the real kernel and rootfs into a fresh dir so a test
// can place an images.lock beside them without touching build/artifacts.
func copyImagesToTemp(t *testing.T) (kernel, rootfs string) {
	t.Helper()
	m, err := config.NewMachine("imgverify-tpl")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Skipf("images missing: %v", err)
	}
	dir := t.TempDir()
	kernel = filepath.Join(dir, "vmlinux")
	rootfs = filepath.Join(dir, "rootfs.ext4")
	for _, c := range []struct{ src, dst string }{
		{m.KernelImagePath, kernel}, {m.RootFSPath, rootfs},
	} {
		b, err := os.ReadFile(c.src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(c.dst, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return kernel, rootfs
}

func startServerImageVerify(t *testing.T, mode, kernel, rootfs string) (*server.Server, error) {
	t.Helper()
	log := logrus.New()
	log.SetLevel(logrus.ErrorLevel)
	return server.New(server.Config{
		Templates:   []server.Template{{Name: "default", Kernel: kernel, RootFS: rootfs, VCPUs: 1, MemMiB: 256}},
		Tokens:      map[string]string{"t1": "s1"},
		ImageVerify: mode,
		Version:     "test",
	}, log)
}

func TestImageVerifyEnforcePasses(t *testing.T) {
	kernel, rootfs := copyImagesToTemp(t)
	writeImagesLock(t, kernel, rootfs)
	srv, err := startServerImageVerify(t, "enforce", kernel, rootfs)
	if err != nil {
		t.Fatalf("enforce with a matching lock must start: %v", err)
	}
	t.Cleanup(func() {
		sc, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(sc)
	})
}

func TestImageVerifyMismatchRefuses(t *testing.T) {
	kernel, rootfs := copyImagesToTemp(t)
	writeImagesLock(t, kernel, rootfs)
	// Corrupt the rootfs after the lock is written.
	f, err := os.OpenFile(rootfs, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("tamper")
	f.Close()

	if _, err := startServerImageVerify(t, "enforce", kernel, rootfs); err == nil {
		t.Fatal("enforce with a digest mismatch must refuse to start")
	}
	// warn mode starts despite the mismatch.
	srv, err := startServerImageVerify(t, "warn", kernel, rootfs)
	if err != nil {
		t.Fatalf("warn mode must start on a mismatch: %v", err)
	}
	sc, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(sc)
}

func TestImageVerifyNoLockStarts(t *testing.T) {
	kernel, rootfs := copyImagesToTemp(t)
	// No images.lock anywhere near the temp dir.
	srv, err := startServerImageVerify(t, "enforce", kernel, rootfs)
	if err != nil {
		t.Fatalf("a missing lock must not stop startup: %v", err)
	}
	sc, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(sc)
}
```

Add `"crypto/sha256"`, `"encoding/hex"`, `"io"`, `"os"`, `"path/filepath"` to the test file's imports if they are not already present.

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test -tags integration ./internal/server/ -run 'TestImageVerify' -v`
Expected: FAIL — `server.Config` has no `ImageVerify` field, so the test file does not compile.

- [ ] **Step 3: Add the Config fields**

In `internal/server/server.go`, inside `type Config struct`, after the `ConfigSHA256 string` field, add:

```go
	// ImageVerify controls the startup image check: "enforce" (default),
	// "warn", or "off". A missing images.lock is treated as "warn" in every
	// mode.
	ImageVerify string
	// CosignIdentity and CosignIssuer, when set, make the check also verify
	// images.lock.sig with `cosign verify-blob`.
	CosignIdentity string
	CosignIssuer   string
```

- [ ] **Step 4: Write `internal/server/images_verify.go`**

Create `internal/server/images_verify.go`:

```go
package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"

	"github.com/sirupsen/logrus"

	"github.com/M30W-M1X/moat/internal/imagelock"
)

// imageResult is one template's image-verification outcome. Start puts these in
// the daemon.start audit event.
type imageResult struct {
	Template     string
	KernelSHA256 string
	RootFSSHA256 string
	SBOMSHA256   string
	Verified     bool
	Signed       bool
}

// verifyImages checks every template's kernel and rootfs against the images.lock
// beside them. In "enforce" mode a mismatch marks that template unusable, and an
// all-templates-unusable result returns an error. In "warn" mode a mismatch only
// logs. In "off" mode the check does not run. A missing lock always downgrades
// to a warning.
func verifyImages(cfg Config, log *logrus.Logger) ([]imageResult, error) {
	mode := cfg.ImageVerify
	if mode == "" {
		mode = "enforce"
	}
	results := make([]imageResult, 0, len(cfg.Templates))
	usable := 0

	for _, t := range cfg.Templates {
		r := imageResult{Template: t.Name}
		if sum, err := sha256File(t.Kernel); err == nil {
			r.KernelSHA256 = sum
		}
		if sum, err := sha256File(t.RootFS); err == nil {
			r.RootFSSHA256 = sum
		}

		if mode == "off" {
			results = append(results, r)
			usable++
			continue
		}

		lockPath := imagelock.Find(t.RootFS)
		if lockPath == "" {
			log.WithField("template", t.Name).
				Warn("no images.lock beside the rootfs, the image check is skipped")
			results = append(results, r)
			usable++
			continue
		}

		lock, err := imagelock.Load(lockPath)
		if err != nil {
			if mode == "enforce" {
				log.WithError(err).WithField("template", t.Name).Error("images.lock is unreadable")
				results = append(results, r)
				continue
			}
			log.WithError(err).WithField("template", t.Name).Warn("images.lock is unreadable")
			results = append(results, r)
			usable++
			continue
		}
		r.SBOMSHA256 = lock.SBOM.SHA256

		if err := lock.VerifyFiles(t.Kernel, t.RootFS); err != nil {
			var me *imagelock.MismatchError
			if errors.As(err, &me) {
				log.WithFields(logrus.Fields{
					"template": t.Name, "file": me.File, "want": me.Want, "got": me.Got,
				}).Error("image digest mismatch")
			} else {
				log.WithError(err).WithField("template", t.Name).Error("image verification failed")
			}
			if mode == "warn" {
				results = append(results, r)
				usable++
			} else {
				results = append(results, r)
			}
			continue
		}

		if err := lock.VerifySignature(lockPath, lockPath+".sig", cfg.CosignIdentity, cfg.CosignIssuer); err != nil {
			log.WithError(err).WithField("template", t.Name).Error("images.lock signature check failed")
			if mode == "warn" {
				r.Verified = true
				results = append(results, r)
				usable++
			} else {
				results = append(results, r)
			}
			continue
		}

		r.Verified = true
		r.Signed = cfg.CosignIdentity != ""
		results = append(results, r)
		usable++
		log.WithFields(logrus.Fields{"template": t.Name, "signed": r.Signed}).
			Info("image verified against images.lock")
	}

	if mode == "enforce" && usable == 0 {
		return results, errors.New("no template passed image verification")
	}
	return results, nil
}

// imageResultsAttr renders the results for the daemon.start audit event.
func imageResultsAttr(rs []imageResult) []map[string]any {
	out := make([]map[string]any, 0, len(rs))
	for _, r := range rs {
		out = append(out, map[string]any{
			"template":      r.Template,
			"kernel_sha256": r.KernelSHA256,
			"rootfs_sha256": r.RootFSSHA256,
			"sbom_sha256":   r.SBOMSHA256,
			"verified":      r.Verified,
			"signed":        r.Signed,
		})
	}
	return out
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
```

- [ ] **Step 5: Call `verifyImages` from `New` and stash the results**

In `internal/server/server.go`, add a field to `type Server struct`:

```go
	imageResults []imageResult
```

In `New`, after the template map is built (`for _, t := range cfg.Templates { s.tmpl[t.Name] = t }`) and before the secret-scanner block, add:

```go
	res, err := verifyImages(cfg, log)
	if err != nil {
		return nil, err
	}
	s.imageResults = res
```

`New` already has an `err` in scope from the `netip.ParsePrefix` call, so use `=` not `:=` if the linter complains about shadowing — match the file's existing style (it uses `err :=` repeatedly, so `res, err := verifyImages(...)` is fine).

- [ ] **Step 6: Add the `images` attr to `daemon.start`**

In `Start`, in the `s.audit.RecordSync(ctx, audit.Event{Type: audit.TypeDaemonStart, Attrs: map[string]any{...}})` call, add one line to the `Attrs` map:

```go
			"images":        imageResultsAttr(s.imageResults),
```

- [ ] **Step 7: Run the new tests**

Run: `go test -tags integration ./internal/server/ -run 'TestImageVerify' -v`
Expected: PASS for `TestImageVerifyEnforcePasses`, `TestImageVerifyMismatchRefuses`, `TestImageVerifyNoLockStarts`.

- [ ] **Step 8: Run the wider audit suite to catch regressions**

Run: `go test -tags integration ./internal/server/ -run 'TestAudit|TestImageVerify' -v`
Expected: all pass. The existing audit tests still pass because the temp-dir templates they use have no `images.lock`, so verification downgrades to a warning.

- [ ] **Step 9: Build, vet, format**

```bash
go build ./... && go vet ./... && go vet -tags integration ./...
gofmt -l internal/server/
```

Expected: clean.

- [ ] **Step 10: Commit**

```bash
git add internal/server/images_verify.go internal/server/server.go internal/server/server_integration_test.go
git commit -m "$(cat <<'EOF'
feat(server): verify kernel and rootfs against images.lock at startup

verifyImages runs once in server.New. In enforce mode (the default) a
digest mismatch drops that template, and an all-templates-unusable
result stops the daemon. warn mode logs and serves. off mode skips. A
missing images.lock downgrades to a warning in every mode. The
daemon.start audit event gains an "images" attribute with the digests.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01PRvf4bxSFzyL2dAPgorw7M
EOF
)"
```

---

### Task 8: `moatd` config, flags, and env wiring

**Files:**
- Modify: `cmd/moatd/main.go` (three flags, three env reads, precedence, pass into `cfg`)
- Modify: `cmd/moatd/config.go` (three file keys, validation)
- Modify: `cmd/moatd/config_test.go` (a validation test)

**Interfaces:**
- Consumes: `server.Config.ImageVerify / CosignIdentity / CosignIssuer` from Task 7.
- Produces: nothing new for later tasks.

- [ ] **Step 1: Write the failing config test**

In `cmd/moatd/config_test.go`, add:

```go
func TestConfigImageVerifyValidation(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	bad := filepath.Join(dir, "bad.json")
	base := `{"templates":[{"name":"default"}],"tokens":[{"id":"a","secret":"b"}]`
	if err := os.WriteFile(good, []byte(base+`,"image_verify":"warn"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte(base+`,"image_verify":"loud"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfigFile(good, "k", "r"); err != nil {
		t.Fatalf("image_verify=warn should load: %v", err)
	}
	if _, err := loadConfigFile(bad, "k", "r"); err == nil {
		t.Fatal("image_verify=loud should be rejected")
	}
}
```

Add `"path/filepath"` and `"os"` to the test imports if missing.

- [ ] **Step 2: Run it to see it fail**

Run: `go test ./cmd/moatd/ -run 'TestConfigImageVerify' -v`
Expected: FAIL — `image_verify` is an unknown field (`DisallowUnknownFields`), so `loadConfigFile(good, ...)` errors, which fails the first assertion.

- [ ] **Step 3: Add the file keys**

In `cmd/moatd/config.go`, in `type fileConfig struct`, add:

```go
	ImageVerify    string `json:"image_verify"`
	CosignIdentity string `json:"cosign_identity"`
	CosignIssuer   string `json:"cosign_issuer"`
```

In `type loadedConfig struct`, add:

```go
	ImageVerify    string
	CosignIdentity string
	CosignIssuer   string
```

In `loadConfigFile`, after the `d := fc.Defaults` block and before the audit block, add:

```go
	switch fc.ImageVerify {
	case "", "enforce", "warn", "off":
		out.ImageVerify = fc.ImageVerify
	default:
		return out, fmt.Errorf("%s: image_verify must be enforce, warn, or off", path)
	}
	out.CosignIdentity = fc.CosignIdentity
	out.CosignIssuer = fc.CosignIssuer
```

- [ ] **Step 4: Run the config test to see it pass**

Run: `go test ./cmd/moatd/ -run 'TestConfigImageVerify' -v`
Expected: PASS.

- [ ] **Step 5: Add the flags and env wiring in `main.go`**

In `cmd/moatd/main.go`, with the other `flag.String` calls, add:

```go
	imageVerify := flag.String("image-verify", "", `image check: "enforce" (default), "warn", or "off"`)
	cosignIdentity := flag.String("cosign-identity", "", "cosign certificate identity for images.lock.sig")
	cosignIssuer := flag.String("cosign-issuer", "", "cosign OIDC issuer for images.lock.sig")
```

After `flag.Parse()` and the logger setup, add a small resolver:

```go
	// image_verify precedence: flag, then env, then config file, then default.
	imgVerify := *imageVerify
	if imgVerify == "" {
		imgVerify = os.Getenv("MOAT_IMAGE_VERIFY")
	}
	cosID := *cosignIdentity
	if cosID == "" {
		cosID = os.Getenv("MOAT_COSIGN_IDENTITY")
	}
	cosIss := *cosignIssuer
	if cosIss == "" {
		cosIss = os.Getenv("MOAT_COSIGN_ISSUER")
	}
```

In the `if *cfgFile != "" {` block, after `cfg.ConfigSHA256 = fc.ConfigSHA256`, add:

```go
		if imgVerify == "" {
			imgVerify = fc.ImageVerify
		}
		if cosID == "" {
			cosID = fc.CosignIdentity
		}
		if cosIss == "" {
			cosIss = fc.CosignIssuer
		}
```

After the whole `if *cfgFile != "" { ... } else { ... }` block, before `ctx, stop := signal.NotifyContext(...)`, add:

```go
	switch imgVerify {
	case "", "enforce", "warn", "off":
	default:
		log.Fatalf("image-verify must be enforce, warn, or off, got %q", imgVerify)
	}
	cfg.ImageVerify = imgVerify
	cfg.CosignIdentity = cosID
	cfg.CosignIssuer = cosIss
```

- [ ] **Step 6: Verify the daemon still builds and a bad value exits non-zero**

```bash
go build -o /tmp/moatd ./cmd/moatd
MOAT_TOKENS=ci:s /tmp/moatd -image-verify loud ; echo "exit=$?"
```

Expected: it logs `image-verify must be enforce, warn, or off` and exits non-zero (the `log.Fatalf`).

- [ ] **Step 7: Full check**

```bash
go build ./... && go vet ./... && go test ./cmd/moatd/ -v
gofmt -l cmd/moatd/
```

Expected: clean, tests pass.

- [ ] **Step 8: Commit**

```bash
git add cmd/moatd/main.go cmd/moatd/config.go cmd/moatd/config_test.go
git commit -m "$(cat <<'EOF'
feat(moatd): image_verify config, flags, and env

Add -image-verify / -cosign-identity / -cosign-issuer, the matching
MOAT_IMAGE_VERIFY / MOAT_COSIGN_IDENTITY / MOAT_COSIGN_ISSUER env vars,
and the image_verify / cosign_identity / cosign_issuer keys in
moatd.json. Precedence is flag, env, file, default. A bad image_verify
value stops the daemon at load.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01PRvf4bxSFzyL2dAPgorw7M
EOF
)"
```

---

### Task 9: `moat doctor` image-lock check

**Files:**
- Modify: `cmd/moat/doctor.go` (add the check block and a testable helper)
- Create: `cmd/moat/doctor_test.go`

**Interfaces:**
- Consumes: `imagelock.Find`, `imagelock.Load`, `(*Lock).VerifyFiles`, `config.NewMachine` (already imported in doctor.go).
- Produces: nothing for later tasks.

- [ ] **Step 1: Write the failing test**

Create `cmd/moat/doctor_test.go`:

```go
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func writeF(t *testing.T, p, body string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func hexF(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestImageLockStatus(t *testing.T) {
	dir := t.TempDir()
	kern := filepath.Join(dir, "vmlinux")
	root := filepath.Join(dir, "rootfs.ext4")
	writeF(t, kern, "K")
	writeF(t, root, "R")

	// No lock -> WARN.
	if mark, _ := imageLockStatus(kern, root); mark != "WARN" {
		t.Fatalf("no lock: mark = %q, want WARN", mark)
	}

	lock := filepath.Join(dir, "images.lock")
	writeF(t, lock, `{"schema":"moat.images.lock/v1","kernel":{"path":"vmlinux","sha256":"`+hexF("K")+`"},"rootfs":{"path":"rootfs.ext4","sha256":"`+hexF("R")+`"},"sbom":{"path":"sbom.cdx.json","sha256":"abcd1234"}}`)

	// Matching lock -> PASS.
	if mark, _ := imageLockStatus(kern, root); mark != "PASS" {
		t.Fatalf("matching lock: mark = %q, want PASS", mark)
	}

	// Tampered rootfs -> FAIL.
	writeF(t, root, "R-TAMPERED")
	if mark, _ := imageLockStatus(kern, root); mark != "FAIL" {
		t.Fatalf("mismatch: mark = %q, want FAIL", mark)
	}
}
```

- [ ] **Step 2: Run it to see it fail**

Run: `go test ./cmd/moat/ -run 'TestImageLockStatus' -v`
Expected: FAIL — `imageLockStatus` is undefined.

- [ ] **Step 3: Add the helper and the check**

In `cmd/moat/doctor.go`, add the import `"github.com/M30W-M1X/moat/internal/imagelock"` and this helper at the end of the file:

```go
// imageLockStatus reports the doctor line for the images.lock beside a rootfs.
func imageLockStatus(kernelPath, rootfsPath string) (mark, detail string) {
	p := imagelock.Find(rootfsPath)
	if p == "" {
		return "WARN", "no images.lock beside " + rootfsPath
	}
	l, err := imagelock.Load(p)
	if err != nil {
		return "FAIL", "images.lock unreadable: " + err.Error()
	}
	if err := l.VerifyFiles(kernelPath, rootfsPath); err != nil {
		return "FAIL", err.Error()
	}
	return "PASS", "images.lock verified (sbom " + short(l.SBOM.SHA256) + ")"
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
```

In `runDoctor`, after the `images` check block (ends at line 85), add:

```go
	// images.lock — informational.
	if m, err := config.NewMachine("doctor"); err == nil {
		mark, detail := imageLockStatus(m.KernelImagePath, m.RootFSPath)
		check("image-lock", mark, mark == "PASS", detail)
	}
```

Note: `check` treats a non-`PASS` `mark` of `"FAIL"` as a required failure (it increments `fails`). That is the intended behaviour — a digest mismatch on the local images is worth a non-zero `moat doctor`. `WARN` does not increment `fails`.

- [ ] **Step 4: Run the test to see it pass**

Run: `go test ./cmd/moat/ -run 'TestImageLockStatus' -v`
Expected: PASS.

- [ ] **Step 5: Full check**

```bash
go build ./... && go vet ./... && go test ./cmd/moat/ -v
gofmt -l cmd/moat/
```

Expected: clean, tests pass.

- [ ] **Step 6: Commit**

```bash
git add cmd/moat/doctor.go cmd/moat/doctor_test.go
git commit -m "$(cat <<'EOF'
feat(moat): doctor checks images.lock

moat doctor gains an image-lock line. It reports PASS when the lock
beside the default rootfs verifies, WARN when no lock is present, and
FAIL on a digest mismatch. It is read-only and does not change what
moatd serves.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01PRvf4bxSFzyL2dAPgorw7M
EOF
)"
```

---

### Task 10: Release staging, installer, and docs

**Files:**
- Modify: `build/release.sh` (stage `images.lock` and `sbom.cdx.json`)
- Modify: `install.sh` (install the two files beside the images)
- Modify: `docs/moatd.example.json` (add `image_verify`)
- Modify: `docs/api.md` (document the `daemon.start` `images` attribute)
- Modify: `docs/install.md` (a short "verifying the image" section)
- Modify: `README.md` (one bullet + `internal/imagelock` in the layout table)
- Modify: `CLAUDE.md:92` (the "Image pipeline" line)

**Interfaces:** none — prose and one JSON key.

- [ ] **Step 1: Stage the lock and SBOM in `release.sh`**

In `build/release.sh`, after the `if [ -f "$ROOT/build/artifacts/vmlinux" ] ...` block that copies the images (ends near line 37), add:

```sh
for f in images.lock sbom.cdx.json; do
	if [ -f "$ROOT/build/artifacts/$f" ]; then
		cp "$ROOT/build/artifacts/$f" "$STAGE/share/$f"
	fi
done
```

Also copy the repo-root `images.lock` as the canonical one when the built copy is absent:

```sh
if [ ! -f "$STAGE/share/images.lock" ] && [ -f "$ROOT/images.lock" ]; then
	cp "$ROOT/images.lock" "$STAGE/share/images.lock"
fi
```

- [ ] **Step 2: Install the two files in `install.sh`**

In `install.sh`, change the image loop (around line 132) from:

```sh
for f in vmlinux rootfs.ext4; do
```

to:

```sh
for f in vmlinux rootfs.ext4 images.lock sbom.cdx.json; do
```

The loop body already handles "present → install, absent → note", so `images.lock` and `sbom.cdx.json` follow the same rule.

- [ ] **Step 3: Add `image_verify` to the example config**

In `docs/moatd.example.json`, add a top-level key after the `audit` block:

```json
  "image_verify": "enforce",
```

- [ ] **Step 4: Document the `daemon.start` attribute in `docs/api.md`**

In `docs/api.md`, in the audit-log section where event shapes are described, add:

```markdown
`daemon.start` also carries an `images` array — one entry per template with
`kernel_sha256`, `rootfs_sha256`, `sbom_sha256`, `verified`, and `signed`.
`moatd` computes these at startup from `images.lock` beside each template's
rootfs. In the default `enforce` mode a digest mismatch drops that template,
and `moatd` exits non-zero when no template is left. Set `image_verify` to
`warn` or `off`, or `MOAT_IMAGE_VERIFY`, to change that.
```

- [ ] **Step 5: Add a "verifying the image" section to `docs/install.md`**

Add:

```markdown
## Verifying the image

The release ships `images.lock` and `sbom.cdx.json` beside `vmlinux` and
`rootfs.ext4`. `images.lock` records the sha256 of each file plus the base
image digest, the kernel snapshot, and the SBOM digest.

Check the images by hand:

```
cd /var/lib/moat
sha256sum -c <(python3 - <<'PY'
import json
d = json.load(open("images.lock"))
print(d["kernel"]["sha256"], " ", d["kernel"]["path"])
print(d["rootfs"]["sha256"], " ", d["rootfs"]["path"])
print(d["sbom"]["sha256"], " ", d["sbom"]["path"])
PY
)
```

`moatd` runs the same check at startup. In the default `enforce` mode it will
not serve a template whose kernel or rootfs does not match `images.lock`. If
the release also ships `images.lock.sig`, set `cosign_identity` (or
`MOAT_COSIGN_IDENTITY`) and `moatd` verifies the signature too.

Rebuild the rootfs yourself and confirm it matches:

```
make images && make images-verify
```

This is byte-for-byte reproducible on one toolchain (the same `e2fsprogs`,
`podman`, `go`, and `tar`). On a different toolchain the package set and the
SBOM still match, but the ext4 byte layout can differ.
```

- [ ] **Step 6: Update `README.md`**

Add a bullet under the M2 items:

```markdown
- **Reproducible images (M2)** — `make images` builds a byte-identical
  `rootfs.ext4` on one toolchain, pins the kernel by sha256, and writes
  `images.lock` plus a CycloneDX `sbom.cdx.json`. `moatd` verifies the kernel
  and rootfs against `images.lock` at startup and fails closed on a mismatch
  (`image_verify=enforce`, the default). `make images-verify` proves the
  rebuild. `internal/imagelock`.
```

Add to the Layout table:

```markdown
| `internal/imagelock` | load and verify images.lock; the moatd startup image check |
```

- [ ] **Step 7: Fix the `CLAUDE.md` image-pipeline line**

In `CLAUDE.md`, change line 92 from:

```markdown
- **Image pipeline:** `Dockerfile` → `docker export` → mounted `mkfs.ext4` → inject agent.
```

to:

```markdown
- **Image pipeline:** pinned base image `@sha256` → `podman export` →
  re-stamped tree → `mkfs.ext4 -d` with a fixed UUID and hash seed → inject
  the agent. Deterministic on one toolchain. `build/gen-images-lock.sh` writes
  `images.lock`; `go run ./build/sbomgen` writes a CycloneDX SBOM; `moatd`
  verifies both at startup.
```

- [ ] **Step 8: Verify nothing is broken**

```bash
go build ./... && go vet ./...
python3 -c 'import json; json.load(open("docs/moatd.example.json"))'
gofmt -l .
```

Expected: build and vet clean, the JSON parses, `gofmt -l` prints nothing.

- [ ] **Step 9: Commit**

```bash
git add build/release.sh install.sh docs/moatd.example.json docs/api.md docs/install.md README.md CLAUDE.md
git commit -m "$(cat <<'EOF'
docs: images.lock in the release, the installer, and the docs

release.sh and install.sh carry images.lock and sbom.cdx.json beside
the images. Document image_verify, the daemon.start images attribute,
and how a user checks an image by hand or with `make images-verify`.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01PRvf4bxSFzyL2dAPgorw7M
EOF
)"
```

---

## Final verification

- [ ] `gofmt -l .` prints nothing.
- [ ] `go vet ./...` and `go vet -tags integration ./...` are clean.
- [ ] `go build ./...` succeeds.
- [ ] `go test ./...` passes (includes `internal/imagelock`, `build/sbomgen`, `cmd/moat`, `cmd/moatd`).
- [ ] `go test -tags integration ./internal/server/ -run 'TestAudit|TestImageVerify' -v` passes.
- [ ] `go mod tidy` leaves `go.mod` / `go.sum` unchanged except the added `toolchain` line — no new dependency.
- [ ] `make images` produces `vmlinux`, `rootfs.ext4`, `sbom.cdx.json`, and `images.lock` in `build/artifacts/`.
- [ ] `make images-verify` prints `reproducible: OK`.
- [ ] `build/artifacts/images.lock` matches the checked-in repo-root `images.lock` except `generated_at`.
- [ ] `moat doctor` shows an `image-lock` line (`PASS` when the local images match).
- [ ] Manual: `MOAT_TOKENS=ci:s moatd` starts, `daemon.start` in the audit log has an `images` array with `verified:true`.
- [ ] Manual: append a byte to `build/artifacts/rootfs.ext4`, start `moatd` → it refuses (`enforce`), and `moatd -image-verify warn` starts with an error log.
- [ ] Update ClaudeVault `Build Log.md` with a "M2 — Reproducible image builds" entry, mirror a one-line note into agentcairn, and add any footgun to `Gotchas.md` as `G-N` (candidates: `mkfs.ext4` determinism flags, `podman export` timestamp normalisation, `debugfs -R` to read the apk db).

## Self-review notes (from writing this plan)

- **Spec coverage:** Non-goals (no kernel-from-source, no verify-image CLI, no per-sandbox event, no CI) are respected — no task adds them. Component 1 → Task 2. Component 2 → Task 3. Component 3 → Task 4. Component 4 → Task 5. Component 5 → Task 6. Component 6 → Tasks 7 (server), 8 (moatd config), 9 (doctor). Component 7 → Tasks 5 (Makefile targets) and 10 (release/installer). Rollout and the `daemon.start` change → Task 7. Testing section → per-task tests plus the final verification list.
- **`Lock.SchemaField` vs the `Schema` constant:** the JSON key is `schema`; the Go field is `SchemaField` to avoid a name clash with `const Schema`. Task 7 and Task 9 only read `l.Rootfs.SHA256`, `l.Kernel.SHA256`, `l.SBOM.SHA256`, so the field name does not leak into their code. The Interfaces block in Task 6 names it `Schema string` for brevity; the code and tests use `SchemaField`.
- **`sha256File` defined twice:** once in `internal/server/images_verify.go` and once (as `sha256OfFile`, a `*testing.T` helper) in the server test file, and once in `build/sbomgen/main.go`. These are separate packages, so there is no collision. `internal/imagelock` has its own unexported `sha256File`. This is acceptable duplication of a five-line helper across package boundaries — a shared helper package is not worth it.
- **Existing integration tests:** `startServerWithAudit` builds templates that point at `build/artifacts/{vmlinux,rootfs.ext4}`. After Task 5, `make test-integration` runs `make images` first, so a real `images.lock` sits beside those files. `verifyImages` in the default `enforce` mode will then verify them and pass (the lock matches the freshly built images). If a developer runs the server tests without `make images`, `config.NewMachine().Validate()` already `t.Skip`s them. Either way the existing tests keep working.
- **`E2FSPROGS_FAKE_TIME`:** honoured by `mke2fs` when it is built with `--enable-libblkid`/fake-time support, which the Fedora and Debian packages are. If a build host's `mkfs.ext4` ignores it, `images-verify` catches the drift on that host and the fix is to record the local mkfs behaviour — the plan already logs `mkfs_version` in the lock for exactly this.
- **`toolchain go1.27.0` vs `GOTOOLCHAIN=local`:** the Makefile pins `GOTOOLCHAIN ?= local`, so `go` never downloads a toolchain and uses the installed 1.27.0. The `toolchain` line in `go.mod` is the declared floor for anyone building outside `make`. Task 1 Step 2 makes the implementer confirm the local `go` is exactly 1.27.0.
