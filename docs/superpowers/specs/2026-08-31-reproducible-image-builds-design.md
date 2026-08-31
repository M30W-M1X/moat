# Reproducible Image Builds — Design

**Status:** approved for planning (2026-08-31)
**Milestone:** M2 "Production & fleet" → "Observability & trust" slice
**Branch:** `m2-reproducible-images` (off `main` @ `5086fd1`)

## Goal

Produce a byte-identical `rootfs.ext4` from the same inputs on the same
toolchain, pin the kernel to an exact artifact, and ship a CycloneDX SBOM plus
a checked-in lockfile. `moatd` verifies the kernel and rootfs against the
lockfile at startup and fails closed on a mismatch. A user can then inspect
the SBOM and the lockfile with standard tools and trust that the image a
sandbox boots is the image the source produces.

## Non-goals

- Building the kernel from source. The kernel is a pinned, hash-checked
  download from the Firecracker CI bucket.
- Byte-identical rootfs across different toolchains. The guarantee holds for a
  fixed `e2fsprogs` / `podman` / `go` / `tar`. Across versions the package set
  and the SBOM stay identical, but the ext4 byte layout may shift. `moatd`
  enforces the digest from the lockfile that shipped with the image, so this
  does not weaken the runtime check.
- A `moat verify-image` CLI subcommand. Verification runs in `moatd` at
  startup. `moat doctor` gains a read-only visibility check.
- Per-sandbox image-digest audit events. Only `daemon.start` changes.
- A CI pipeline. `make images-verify` is the repeatable check. A one-line
  follow-up covers wiring it into CI when CI exists.

## Global constraints

- No new Go module dependency. CycloneDX output is hand-marshaled structs.
  `cosign` is an optional external binary, invoked only when configured.
- Comments and log/error strings follow ASD-STE100 (active voice, one
  instruction per sentence, ≤20 words for instructions, no semicolons, no
  phrasal verbs, simple present).
- The rootfs build stays rootless (Gotcha G-4): no loop mount. Read one file
  out of the ext4 with `debugfs -R`, not a mount.
- Go 1.27. Add `toolchain go1.27.0` to `go.mod`.

## Architecture and data flow

```
make images
 ├─ fetch-kernel.sh   → artifacts/vmlinux           pinned key, sha256-checked
 ├─ build-rootfs.sh   → artifacts/rootfs.ext4       SOURCE_DATE_EPOCH, fixed fs UUID + hash seed
 ├─ sbomgen           → artifacts/sbom.cdx.json     reads rootfs apk db + guest-agent buildinfo
 └─ gen-images-lock   → artifacts/images.lock       sha256 of vmlinux, rootfs, sbom + provenance
                        artifacts/images.lock.sig   optional, cosign sign-blob, done out of band

moatd start
 └─ for each distinct (kernel, rootfs) pair across templates:
     ├─ imagelock.Find(rootfsPath)     lock beside the rootfs, else parent dir, else ""
     ├─ lock absent                    → warn once, serve  (back-compat)
     ├─ lock present                   → sha256(kernel), sha256(rootfs) must equal the lock
     │                                    else drop that template
     ├─ lock.sig + cosign identity set → cosign verify-blob, else drop that template
     └─ no template survives in enforce mode → moatd exits non-zero
 └─ daemon.start audit attrs gain:
     images: [{template, kernel_sha256, rootfs_sha256, sbom_sha256, verified, signed}]
```

`artifacts/` is git-ignored. The built blobs live there. A canonical
`images.lock` is checked in at the repo root. `make images-verify` diffs the
freshly built lock against the repo-root copy. The release tarball carries
`images.lock` and `sbom.cdx.json`; `install.sh` puts them in `<data>/`
(default `/var/lib/moat/`) beside `vmlinux` and `rootfs.ext4`.

## Component 1 — `build/fetch-kernel.sh` (pin the kernel)

Today the script resolves "newest CI snapshot" and "newest `vmlinux-6.1.x`"
dynamically. Two runs a month apart get different kernels.

Change:

- Add two constants near the top:
  `KERNEL_CI_SNAPSHOT` (for example `20260826-761f88fbb951-0`) and
  `KERNEL_VERSION_EXACT` (for example `6.1.150`). Both overridable by env.
- Build the key directly:
  `firecracker-ci/${KERNEL_CI_SNAPSHOT}/${ARCH}/vmlinux-${KERNEL_VERSION_EXACT}`.
  Remove the bucket-listing and `sort | tail -1` logic.
- Add a constant `KERNEL_SHA256`. After download, compute the sha256 and
  compare. On mismatch, remove the file and exit non-zero.
- Keep the `.config` sibling download (informational).
- Keep the "already present" short-circuit, but re-verify the sha256 of the
  existing file and re-download on mismatch.

The exact snapshot, version, URL, and sha256 also go into `images.lock`
(`kernel` block). Bumping the kernel is a deliberate edit of these four
constants plus a regenerated lock.

Gotcha reference: this closes the "until we build our own kernel" caveat on
G-3 by pinning instead of resolving.

## Component 2 — `build/build-rootfs.sh` (deterministic rootfs)

### Determinism controls

| Source of non-determinism | Control |
|---|---|
| `podman pull alpine:3.20` (moving tag) | Constant `BASE_IMAGE=docker.io/library/alpine@sha256:<digest>`. Recorded in the lock. |
| `podman export` tar: per-file mtime, ctime, entry order | Extract, then re-stamp the whole tree: `find "$root" -exec touch --no-dereference --date=@"$SOURCE_DATE_EPOCH" {} +`. |
| guest-agent binary: build path, VCS stamp, build id | `CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags='-s -w -buildid='`. `toolchain go1.27.0` in `go.mod`. |
| `mkfs.ext4`: random fs UUID, random dir-hash seed, superblock and inode times, lazy init | `mkfs.ext4 -q -F -L rootfs -U "$FS_UUID" -E hash_seed="$HASH_SEED",lazy_itable_init=0,lazy_journal_init=0 -d "$root" "$OUT" "$ROOTFS_SIZE"` with `E2FSPROGS_FAKE_TIME=$SOURCE_DATE_EPOCH` in the environment. `FS_UUID` and `HASH_SEED` are constants in the script. |
| `ROOTFS_SIZE` differs between callers (Makefile `256M`, script default `128M`) | Freeze one value (`256M`) in the script. The Makefile stops passing the size. Recorded in the lock. |
| e2fsprogs version drift in block-group packing | Not fully controllable. The lock records `mke2fs --version`. `images-verify` claims reproducibility on the same toolchain only. |

### Constants block (script header)

```
SOURCE_DATE_EPOCH  default: the commit date of the last build/ pipeline change,
                   as a fixed integer literal, overridable by env
BASE_IMAGE         docker.io/library/alpine@sha256:<digest>
ROOTFS_SIZE        256M
FS_UUID            a fixed UUIDv4 literal, for example 5a5a5a5a-0000-4000-8000-000000000001
HASH_SEED          a fixed UUID-format literal for -E hash_seed=
```

### Preflight

Keep the existing `podman` / `mkfs.ext4` / agent-binary checks. Add a check
for `debugfs` (used by `sbomgen`) — warn, do not fail, since `sbomgen` runs
as a separate step.

## Component 3 — `build/sbomgen` (CycloneDX SBOM)

A small Go program at `build/sbomgen/main.go`, run with `go run ./build/sbomgen`,
never built into a shipped binary.

Flags:
`-rootfs artifacts/rootfs.ext4 -agent bin/guest-agent -kernel artifacts/vmlinux`
`-kernel-version 6.1.150 -kernel-snapshot 20260826-... -base-image docker.io/library/alpine@sha256:...`
`-source-date-epoch <int> -out artifacts/sbom.cdx.json`.

Steps:

1. Extract the apk database from the ext4 without a mount:
   `debugfs -R 'cat /lib/apk/db/installed' <rootfs>` → parse the `P:` / `V:` /
   `L:` / `c:` / `A:` records.
2. Read the guest-agent module list: `go version -m <agent>` → parse `dep` and
   `mod` lines.
3. Build the CycloneDX 1.5 document as Go structs and marshal with sorted keys
   (a fixed field order in the struct plus `json.Marshal`, which sorts map
   keys — no maps in the doc, so struct field order is the canonical order).

Document shape (CycloneDX 1.5, `bomFormat: "CycloneDX"`, `specVersion: "1.5"`):

- `metadata.timestamp`: RFC3339 built from `-source-date-epoch` (informational,
  excluded from `images-verify` byte comparison — see note below).
- `metadata.component`: the rootfs image, `type: "container"`,
  `name: "moat-rootfs"`, a `hashes` entry with its sha256.
- `components`:
  - one `operating-system` component for the Alpine base,
    `purl: pkg:oci/alpine@<digest>`.
  - one `library` per apk package,
    `purl: pkg:apk/alpine/<name>@<version>?arch=<arch>`, `licenses` from `L:`.
  - one `application` component `guest-agent` and one `library` per Go module,
    `purl: pkg:golang/<module>@<version>`.
  - one `data` component for the kernel: `name: "vmlinux"`,
    `version: "<kernel-version>"`, a `properties` entry for the CI snapshot,
    a `hashes` entry with its sha256.
- Components sorted by `purl` then `name` for a stable order.

Determinism note: `metadata.timestamp` is derived only from
`-source-date-epoch`, so two runs with the same inputs produce a
byte-identical SBOM. The build passes the same epoch that `build-rootfs.sh`
used.

No new dependency: the CycloneDX structs are defined in
`build/sbomgen/cyclonedx.go`.

## Component 4 — `build/gen-images-lock.sh` and `images.lock`

`build/gen-images-lock.sh` runs after the rootfs, the SBOM, and the kernel are
in `artifacts/`. It computes the three sha256 sums and writes the JSON.

`images.lock` (`schema: "moat.images.lock/v1"`):

```json
{
  "schema": "moat.images.lock/v1",
  "source_date_epoch": 1756512000,
  "kernel": {
    "path": "vmlinux",
    "sha256": "<hex>",
    "ci_snapshot": "20260826-761f88fbb951-0",
    "version": "6.1.150",
    "source": "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/20260826-761f88fbb951-0/x86_64/vmlinux-6.1.150"
  },
  "rootfs": {
    "path": "rootfs.ext4",
    "sha256": "<hex>",
    "size_bytes": 268435456,
    "base_image": "docker.io/library/alpine@sha256:<digest>",
    "fs_uuid": "5a5a5a5a-0000-4000-8000-000000000001",
    "mkfs_version": "mke2fs 1.47.1"
  },
  "sbom": { "path": "sbom.cdx.json", "sha256": "<hex>" },
  "guest_agent": { "sha256": "<hex>", "go": "go1.27.0" },
  "generated_by": "moat build pipeline",
  "generated_at": "2026-08-31T00:00:00Z"
}
```

`generated_at` is informational and the only non-reproducible field.
`images-verify` compares every field except `generated_at`. `moatd` enforces
`kernel.sha256`, `rootfs.sha256`, and `sbom.sha256`.

`images.lock.sig` is produced out of band by the release owner:
`cosign sign-blob --yes --output-signature images.lock.sig images.lock`.
It is not produced by `make images`.

The checked-in repo-root `images.lock` is the canonical reference. `make
images` writes `artifacts/images.lock`; `make images-verify` diffs the two
(minus `generated_at`) and fails on drift, so a pipeline change that alters a
digest forces a deliberate repo-root update.

## Component 5 — `internal/imagelock`

New package. No dependency beyond the standard library and `os/exec` for
`cosign`.

```go
package imagelock

type Lock struct {
    Schema          string     `json:"schema"`
    SourceDateEpoch int64      `json:"source_date_epoch"`
    Kernel          FileEntry  `json:"kernel"`
    Rootfs          RootfsEntry `json:"rootfs"`
    SBOM            FileRef    `json:"sbom"`
    GuestAgent      AgentEntry `json:"guest_agent"`
    GeneratedBy     string     `json:"generated_by"`
    GeneratedAt     string     `json:"generated_at"`
}

// Load reads and validates a lock file. It rejects an unknown schema.
func Load(path string) (*Lock, error)

// Find returns the images.lock path for a rootfs, or "" when none exists.
// It looks in the rootfs directory first, then the parent directory.
func Find(rootfsPath string) string

// VerifyFiles hashes kernelPath and rootfsPath and compares them to the lock.
// A mismatch error names the file and both digests.
func (l *Lock) VerifyFiles(kernelPath, rootfsPath string) error

// VerifySignature runs `cosign verify-blob` on sigPath against identity and
// issuer. It returns nil when cosign is absent and identity is empty. It
// returns an error when identity is set and verification fails.
func (l *Lock) VerifySignature(lockPath, sigPath, identity, issuer string) error

type MismatchError struct{ File, Want, Got string }
func (e *MismatchError) Error() string
```

`VerifyFiles` reads each file in a streaming sha256 (`io.Copy` into
`sha256.New()`), so a 256 MiB rootfs does not load into memory.

## Component 6 — `moatd` wiring

### Config

`internal/server.Config` gains:

```go
ImageVerify   string // "enforce" (default), "warn", "off"
CosignIdentity string
CosignIssuer   string
```

`cmd/moatd`:

- Flags: `-image-verify`, `-cosign-identity`, `-cosign-issuer`.
- Env: `MOAT_IMAGE_VERIFY`, `MOAT_COSIGN_IDENTITY`, `MOAT_COSIGN_ISSUER`.
- `moatd.json` top-level keys: `image_verify`, `cosign_identity`,
  `cosign_issuer`.
- Precedence, highest first: flag, env, config file, default.
- Validation: `image_verify` must be one of the three values, else `moatd`
  exits non-zero at load.

### Startup gate

In `server.New`, after templates resolve and before the warm pool starts:

1. Collect the distinct `(kernelPath, rootfsPath)` pairs across all templates.
2. For each pair:
   - `p := imagelock.Find(rootfsPath)`.
   - `p == ""` → record `verified:false, signed:false` for every template on
     that pair. Log one warning. Continue. (Applies in every mode, so an
     existing install without a lock still starts.)
   - `p != ""` → `imagelock.Load(p)`, then `VerifyFiles`. On error:
     - `enforce` → mark every template on that pair unusable.
     - `warn` → log at error level, record `verified:false`.
     - `off` → skip verification entirely (do not even load the lock).
   - If a `.sig` sits beside the lock and `CosignIdentity != ""` →
     `VerifySignature`. Same mode handling. Success records `signed:true`.
3. `enforce` mode, and every template is unusable → `server.New` returns an
   error; `cmd/moatd` logs it and exits non-zero.
4. Build the `daemon.start` `images` attribute from the per-template results.

### Audit

`daemon.start` attrs gain one key:

```json
"images": [
  {"template": "default", "kernel_sha256": "<hex>", "rootfs_sha256": "<hex>",
   "sbom_sha256": "<hex>", "verified": true, "signed": false}
]
```

Metadata only — digests, not contents. No new event type. No `sandbox.create`
change.

### `moat doctor`

Add an `image-lock` check. For the default template's rootfs:

- lock found and `VerifyFiles` passes → `OK  images.lock verified (sbom <short-sha>)`
- lock not found → `WARN  no images.lock beside <rootfs>`
- `VerifyFiles` fails → `FAIL  <file> digest mismatch`

Read-only. It never changes what `moatd` serves. It exists because there is no
`verify-image` subcommand.

## Component 7 — Makefile and release

- `images:` depends on `kernel rootfs sbom lock` (new order). `sbom` runs
  `go run ./build/sbomgen ...`; `lock` runs `build/gen-images-lock.sh`.
- New target `images-verify`:
  1. `make images` (first build) → keep `artifacts/`.
  2. Rebuild the rootfs and the SBOM into a scratch dir.
  3. Assert `sha256(scratch/rootfs.ext4) == sha256(artifacts/rootfs.ext4)`.
  4. Assert both equal `images.lock` `rootfs.sha256`.
  5. Assert `sha256(scratch/sbom.cdx.json) == sha256(artifacts/sbom.cdx.json)`.
  6. Assert `artifacts/images.lock` equals the repo-root `images.lock` with
     `generated_at` removed from both.
  7. Print `reproducible: OK` or the first failing assertion and exit non-zero.
- `build/release.sh` also copies `images.lock` and `sbom.cdx.json` into
  `$STAGE/share/` when they exist. `checksums.txt` already covers the tarball.
- `install.sh` `for f in vmlinux rootfs.ext4` loop gains `images.lock` and
  `sbom.cdx.json` (best-effort — a note, not a failure, when absent, same as
  the images today).

## Testing

### `internal/imagelock` unit tests (no root, no images)

- `Load` round-trips the schema. It rejects an unknown `schema` string and
  malformed JSON.
- `Find`: a lock beside the rootfs wins over one in the parent directory; `""`
  when neither exists.
- `VerifyFiles`: hash two temp files into a `Lock`, assert pass; flip one byte,
  assert a `*MismatchError` that names the file and both digests.
- `VerifySignature`: `cosign` absent and identity empty → nil; identity set and
  `cosign` absent → error. Real signature checks are skipped when `cosign` is
  not on `PATH`.

### `internal/server` integration tests (this host runs them)

- `TestImageVerifyEnforce`: a template rootfs with a matching `images.lock` →
  the server starts and `daemon.start` carries
  `images:[{... "verified": true}]`.
- `TestImageVerifyMismatchRefuses`: append one byte to the rootfs after the
  lock is written. `enforce` → `server.New` returns an error and no template is
  served. `warn` → the server starts and logs at error level.
- `TestImageVerifyNoLock`: no lock present → the server starts, logs one
  warning, and the `daemon.start` `images` entry has `verified:false`.

### `build/sbomgen` unit test

- Feed a captured `/lib/apk/db/installed` fixture and a fake `go version -m`
  blob. Assert the CycloneDX JSON has the expected components, purls, license
  strings, and a stable (sorted) component order. Assert two runs with the
  same inputs produce byte-identical output.

### Build-pipeline check

- `make images-verify` as described in Component 7.
- A `TestReproducibleRootfs` Go test under an `images` build tag that wraps the
  same twice-and-diff logic. It skips when `podman` or `mkfs.ext4` is absent,
  so it can join `make test-integration` later.

## Rollout

- On by default in `enforce` mode. An install that has `images.lock` beside its
  images (new installs, and the next release tarball) gets the check for free.
- An existing install with no `images.lock` starts unchanged, with one
  warning. The operator drops in the `images.lock` from the release to turn on
  enforcement, or sets `image_verify=off`.
- Bumping the kernel or the base image: edit the constants in the two build
  scripts, run `make images`, run `make images-verify`, commit the new
  repo-root `images.lock`. Re-sign `images.lock.sig` at release time.

## Open follow-ups (not this slice)

- Wire `make images-verify` and `go test ./...` into a CI workflow once the
  repo has CI.
- A downloadable per-language template registry (`moat template pull python`)
  will need its own per-template lock and SBOM. Out of scope here.
- Signing the SBOM and lock in an automated release pipeline (currently a
  manual `cosign sign-blob`).
