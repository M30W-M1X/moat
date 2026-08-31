# `moat template import` — design

**Status:** approved 2026-08-31
**Author:** brainstorm session (user + Claude)
**Branch:** `template-import` off `main`

## Goal

Add `moat template import` — one command that turns an existing OCI container
image (registry pull or local archive) into a Moat rootfs template, and
carries the image's `ENV` / `WORKDIR` / `ENTRYPOINT` forward as exec
defaults so a caller does not have to re-specify them on every command.

## Background — how templates work today

- A template is a `moatd.json` entry: `{name, kernel, rootfs, vcpus,
  mem_mib, cpu_max, mem_max_mib, pids_max, disk_bw, net_bw}`. Every field
  is a pointer or a resource cap. A sandbox from a template is "boot this
  rootfs with these limits".
- You create a template by hand-writing `build/templates/<name>/Dockerfile`
  and running `make template-image` (`build/build-template.sh`: `podman
  build` → `podman export | tar` → inject `/sbin/guest-agent` → `mkfs.ext4
  -d`). This needs a repo checkout and `make`.
- `podman export` writes the filesystem only. The image's `ENV`,
  `WORKDIR`, and `ENTRYPOINT` are lost.
- `exec` is fully caller-driven. `execReq` carries `argv`, `env`, `dir`
  per call. The guest agent seeds PID 1 with `PATH`, `HOME`, `TERM`
  (`guest/agent/main.go` `ensureBaseEnv`), then the process gets
  `os.Environ()` + request env (`guest/agent/server.go:54`) and runs in
  `/` unless the request sets `dir`.
- `moat template list` is read-only. `moat template pull` is a
  help-text stub, not implemented.

## What changes

### 1. New way to build a template

`moat template import python:3.12` — no Dockerfile, works from an
installed binary. Also `moat template import --from-archive img.tar` for
a `docker save` tar or an OCI archive.

### 2. Templates gain optional behaviour fields

`env` (map), `workdir`, `default_argv`, and an informational `source`
(`image@sha256:…`). Existing templates set none of these and behave
exactly as today.

### 3. `exec` merges template defaults under the request

- **env** = template `env`, then request `env` keys override, then the
  egress proxy vars (unchanged).
- **dir** = request `dir` if set, else template `workdir`.
- **argv** = request `argv`, or template `default_argv` when the request
  sends none. Empty argv with no `default_argv` is still `400`.

Everything else — boot, per-VM netns / nftables, cgroups, rate limiters,
secret firewall, audit, TTL, warm pool, snapshot — is untouched.

## Global constraints

- **Go 1.27.0 exactly.** `toolchain go1.27.0` in `go.mod`; the Makefile
  exports `GOTOOLCHAIN=local`.
- **No new Go module dependency** without a strong reason. This feature
  adds none — it shells out to `podman` and `mkfs.ext4`, which are
  already required, and parses their output with the standard library.
- **ASD-STE100** for every code comment and every log / error string:
  active voice, one instruction per sentence, ≤ 20 words for
  instructions and ≤ 25 for descriptions, no semicolons, no phrasal
  verbs ("start" not "spin up", "remove" not "take off"), simple tenses.
  Use "start" / "stop" for VM lifecycle.
- **Branch off `main`.** Never merge to `main` without explicit user
  approval.
- **Rootless.** The import pipeline uses `podman export | tar -x` and
  `mkfs.ext4 -d`. No loop mount. No root (Gotcha `G-4`).
- **Determinism knobs match `build/build-rootfs.sh`:**
  `SOURCE_DATE_EPOCH=1788134400`, eager inode / journal init, a fixed
  filesystem UUID and hash seed. The UUID and seed are derived from the
  template name so a re-import of the same name and digest is
  byte-stable.
- **Not written to `images.lock`.** An imported template records the
  image digest in `source` instead. Same rationale as the existing
  `build/templates/`.
- **Bash gotcha `G-18` family:** never pipe a producer into `head -c`
  under `set -o pipefail` — SIGPIPE, exit 141. Not expected here (the
  pipeline is Go), noted for any helper script.

## Architecture

Four units, each testable on its own.

| Unit | Responsibility | Depends on |
| --- | --- | --- |
| `internal/imgimport` (new) | The import pipeline: `Import(ctx, Source, Options) (*Result, error)`. Knows nothing about the CLI or the server. | `podman`, `mkfs.ext4`, `tar`, `du`, `find` as subprocesses; the standard library |
| `cmd/moat` `template import` / `template show` | Argument parsing, source detection, call `imgimport`, register the result (write `moatd.json` or a sidecar + snippet). | `internal/imgimport`, `internal/config` |
| `cmd/moatd/config.go` `fileTemplate` + `internal/server` `Template` | Four new optional fields carried from JSON to the running template. | — |
| `internal/server` `handleExec` + `Sandbox` | Merge the template defaults under the request before `Exec`. | — |

Nothing touches `internal/vm`, the guest agent, `proto/`, `internal/netconf`,
`internal/cgroup`, or `internal/pool`.

## Component detail

### `internal/imgimport`

```go
package imgimport

type Source struct {
	Ref     string // "python:3.12" or "ghcr.io/org/app:v2"; empty when Archive is set
	Archive string // path to a `docker save` tar or an OCI archive
}

type Options struct {
	Name      string // template name and ext4 label; [a-z0-9-], <= 16 chars
	SizeBytes int64  // ext4 size; 0 means auto
	AgentPath string // guest-agent binary; empty means auto-resolve
	OutDir    string // directory for <name>-rootfs.ext4
	SourceDateEpoch int64 // 0 means the default 1788134400
}

type Result struct {
	RootFSPath       string
	Source           string            // "image@sha256:…" from RepoDigests[0]
	Env              map[string]string // image Config.Env
	Dir              string            // image Config.WorkingDir
	DefaultArgv      []string          // Config.Entrypoint ++ Config.Cmd; nil when both empty
	SizeBytes        int64             // the ext4 size used
	Warnings         []string
	PackagesManifest []byte            // apk or dpkg list; nil when neither is present
}

func Import(ctx context.Context, src Source, opts Options) (*Result, error)
```

**Steps inside `Import`:**

1. **Load the image into podman storage.**
   - `src.Ref` set: `podman pull --arch amd64 --os linux <ref>`. Podman
     selects the linux/amd64 manifest from a multi-arch index. A
     single-arch non-amd64 image fails here.
   - `src.Archive` set: `podman load -i <file>`. Read the loaded image
     reference from the command output.
2. **Resolve the digest and the config.**
   `podman inspect --format '{{json .}}' <id>` (or targeted
   `--format` fields). Read `RepoDigests[0]`, `Architecture`, `Os`,
   and `Config`. Assert `Architecture == "amd64" && Os == "linux"`.
   Record `RepoDigests[0]` as `Result.Source`. When `RepoDigests` is
   empty (a `podman load` of an untagged archive), fall back to
   `Id` (the image config digest) and add a warning.
3. **Parse the image config.**
   - `Env`: `Config.Env` (`KEY=VAL` list) → `map[string]string`.
   - `Dir`: `Config.WorkingDir`. An empty string stays unset.
   - `DefaultArgv`: `Config.Entrypoint` ++ `Config.Cmd`. `nil` when
     both are empty (add a warning).
4. **Export the filesystem.**
   `cid=$(podman create <id> /bin/true)`;
   `podman export "$cid" | tar -x -C <work>/root`;
   `podman rm -f "$cid"`.
5. **Inject the agent and the scaffolding.**
   - Resolve the agent: `opts.AgentPath`, else `bin/guest-agent` under
     the working directory, else `<config.ArtifactsDir()>/guest-agent`.
     Fail with a clear message when none exists.
   - `install -Dm0755 <agent> <work>/root/sbin/guest-agent`.
   - `mkdir -p` `proc sys dev tmp run work` under `root`.
   - Write `root/etc/passwd` with a single root line when it is absent.
   - Warn when `root/bin/sh` is missing (only absolute-path execs work).
6. **Normalise and size.**
   - `find <root> -exec touch --no-dereference --date=@<epoch> {} +`.
   - Size = `opts.SizeBytes` when set, else `du -sb <root>` × 1.5,
     rounded up to a whole MiB, with a 64 MiB floor.
7. **Build the ext4.**
   `E2FSPROGS_FAKE_TIME=<epoch> mkfs.ext4 -q -F -L <name> -U <uuid> -E
   hash_seed=<seed>,lazy_itable_init=0,lazy_journal_init=0 -d <root>
   <out>.tmp <size>`, then rename `<out>.tmp` to `<out>` on success.
   `<uuid>` and `<seed>` come from `sha256(name)` sliced into the
   UUID and seed formats, so a re-import of the same name is stable.
8. **Capture `packages.manifest`.**
   Read `root/lib/apk/db/installed` (apk) or `root/var/lib/dpkg/status`
   (dpkg) when present. Best effort. Not an input to the build.

The working directory is a `mktemp -d`, removed on every return path.
`podman rmi` of a pulled image is left in place — the user's podman
cache is theirs to manage.

### `cmd/moat` — `template import` and `template show`

`runTemplate` (`cmd/moat/template.go`) grows `import` and `show` cases.

**`moat template import <ref> [flags]`:**

| Flag | Default | Meaning |
| --- | --- | --- |
| `--name <n>` | derived from the ref | template name and ext4 label |
| `--from-archive <f>` | — | import an archive instead of a pull |
| `--size <N>` | auto | ext4 size, for example `512M` or `2G` |
| `--data <dir>` | `config.ArtifactsDir()` | directory for `<name>-rootfs.ext4` |
| `--agent <path>` | auto-resolve | override the guest-agent binary |
| `--config <moatd.json>` | — | append the template entry to this file |
| `--dry-run` | false | build nothing, print the plan |
| `--json` | false | machine-readable result |

Name derivation: take the last path element of the repository, drop the
tag and digest. `python:3.12` → `python`;
`ghcr.io/org/app:v2` → `app`. Reject a name that is empty, longer than
16 characters, or not `[a-z0-9-]+` after a lower-case fold — tell the
user to pass `--name`.

Steps:
1. Detect the source (`--from-archive` wins; else the positional ref).
2. `--dry-run`: print the resolved name, source, output path, and stop.
3. `imgimport.Import(...)` with `OutDir` = `--data`.
4. Registration:
   - `--config <file>` set: read the file, reject a duplicate `name`,
     append `{name, rootfs, source, env, workdir, default_argv}`
     (`kernel` omitted so the daemon uses its default `vmlinux`),
     re-marshal with two-space indent, write with a temp file and a
     rename.
   - Not set: write `<data>/<name>.template.json` with the same object
     and print the JSON snippet to paste into `moatd.json`.
5. Print a summary: name, source digest, rootfs path and size, the
   captured `env` / `workdir` / `default_argv`, and every warning.

Exit codes: `0` ok, `2` usage, `1` any pipeline or registration failure.

**`moat template show <name>`:** print one template's full detail.
Flags: `--data <dir>` (default `config.ArtifactsDir()`), `--config
<moatd.json>`, `--json`. It reads `<data>/<name>.template.json` first,
then the `--config` file when one is passed, then the built-in
`default`. It exits `1` when the name matches none of them. Output is
human text, or JSON with `--json`.

**`moat template list`:** also list imported templates — scan
`<data>/*.template.json` and `<data>/*-rootfs.ext4`. Show `name`,
`state` (`ready` when the rootfs exists), and the `source` digest when
known. Replace the "planned" `moat template pull` line in the help
text.

### `cmd/moatd/config.go` and `internal/server` `Template`

`fileTemplate` new fields:

```go
Env         map[string]string `json:"env"`          // image ENV, applied under request env
Workdir     string            `json:"workdir"`      // image WORKDIR, used when a request omits dir
DefaultArgv []string          `json:"default_argv"` // image ENTRYPOINT+CMD, used when a request omits argv
Source      string            `json:"source"`       // "image@sha256:…", informational
```

`DisallowUnknownFields` is already set in the loader — the new keys
become known. All fields are optional and default to their zero value,
so every current config parses unchanged.

`server.Template` mirror fields: `Env map[string]string`, `Dir string`,
`DefaultArgv []string`, `Source string`. `loadConfigFile` copies each
across when it builds `[]server.Template`.

`docs/moatd.example.json` gains one imported-style entry (for example a
`python` template with `env`, `workdir`, `default_argv`, `source`).

### `internal/server` `handleExec` and `Sandbox`

`Sandbox` gains a `tpl Template` field, set at create time in
`handleCreate` from the resolved template (the server already has the
`Template` value in scope there — it passes `tpl` to `coldBoot`).

`handleExec`, before the call to `sb.client().Exec`:

```go
tpl := sb.tpl

argv := req.Argv
argvSource := "request"
if len(argv) == 0 {
	argv = tpl.DefaultArgv
	argvSource = "template_default"
}
if len(argv) == 0 {
	writeErr(w, http.StatusBadRequest,
		"argv is required and the template has no default_argv")
	return
}

// env: template first, then request keys override
env := make(map[string]string, len(tpl.Env)+len(req.Env))
for k, v := range tpl.Env {
	env[k] = v
}
for k, v := range req.Env {
	env[k] = v
}

dir := req.Dir
if dir == "" {
	dir = tpl.Dir
}
```

Then:
- The **secret firewall keeps scanning `req.Env` only.** Template env
  comes from a trusted config file, not caller input. This matches the
  existing exemption for the proxy variables.
- The **egress proxy variables** are still forced on the merged `env`
  after the merge, exactly as today, so a template cannot override them.
- The `Exec` call passes `argv`, `env`, `dir` (the merged values), not
  `req.*`.
- **Audit:** `env_keys` in the exec event is the union of template and
  request keys, minus the proxy variables, sorted. Add
  `"argv_source": argvSource` to the exec attrs.

`execReq` JSON shape is unchanged. `moat run` and the MCP `moat_exec`
tool inherit the behaviour with no change.

## Error handling

| Condition | Behaviour |
| --- | --- |
| `podman` not on PATH | fail fast, exit 1, "podman is required for template import" |
| Registry pull fails (no image, or auth) | surface podman stderr, exit 1; hint `podman login` on an auth error |
| Image is not linux/amd64 | exit 1, "image <ref> is <os>/<arch>, need linux/amd64" — checked after the pull and again from `podman inspect` |
| `--from-archive` file missing, unreadable, or not an image archive | exit 1 with the podman `load` error |
| No `/bin/sh` in the image | warning, not a failure; the message notes only absolute-path execs work |
| No `ENTRYPOINT` and no `CMD` | warning, `default_argv` stays null |
| Exported tree larger than `--size` | catch the `mkfs.ext4` failure, re-raise as "image needs more than <size>, pass a larger --size" |
| `--name` collides with an existing template in `--config` or a sidecar | exit 1, "template <name> already exists (use --name)" |
| `tar` / `du` / `find` / `mkfs.ext4` failure | wrapped error that names the step, exit 1; the temp working directory is always removed |
| `--config` write | temp file and atomic rename, the original file is untouched on any failure |
| A crash part way through the build | the rootfs is written to `<name>-rootfs.ext4.tmp` and renamed only on full success |

## Testing

- **`internal/imgimport` unit tests.** Build a small throwaway image
  in-test with `podman build` from an inline Containerfile that sets a
  known `ENV`, `WORKDIR`, and `ENTRYPOINT`. Import it. Assert
  `Result.Env` / `Dir` / `DefaultArgv`, the rootfs file exists,
  `/sbin/guest-agent` is inside it (`debugfs -R`), and `Source` holds a
  digest. Add a `--from-archive` case with `podman save` to a temp tar.
  Skip when `podman` is absent.
- **`cmd/moat` tests.** `template import --dry-run` output. Name
  derivation for `ghcr.io/o/app:v2` → `app`. A `--config` append
  round-trips through `loadConfigFile` with no error. Duplicate-name
  rejection.
- **`cmd/moatd/config.go` test.** A config with `env` / `workdir` /
  `default_argv` loads into `server.Template`. A config without them is
  unchanged.
- **`internal/server` unit test.** The `handleExec` merge: request env
  overrides template env, request dir wins, empty argv falls back to
  `default_argv`, empty argv with no default returns `400`, the audit
  `env_keys` is the union.
- **Integration (`-tags integration`).**
  `imgimport.Import` a pinned `alpine@sha256:…`, register it, start a
  sandbox, `exec` with no `dir` and assert the process ran in the
  image WORKDIR and saw a template env var. A second `exec` with an
  explicit `dir` and env asserts the request won.

## Non-goals for v1

- No signature or SBOM verification of the imported image. The
  `source` digest is recorded. Verifying it is a later change.
- No secret scan of the image contents. The sandbox contains the risk.
- Not added to `images.lock`.
- No `moat run -template <name>` — use `moat run -rootfs <path>`.
- No automatic re-import or update detection. Re-running `import` with
  the same name is a manual, explicit refresh.

## Docs to update

- `build/templates/README.md` — an "Import an existing image" section.
- `docs/developer-guide.md` and `docs/developer-guide.html` — §5 (image
  pipeline) and §7 (using it).
- `README.md` — a line under the demo / templates section.
- `docs/moatd.example.json` — one imported-style template entry.
- `moat template --help` text.
