# microvm_controller

A Go control plane that boots, isolates, and reaps **Firecracker microVMs** as
disposable, hardware-isolated sandboxes for untrusted AI-agent code and tool use.
Greenfield as of 2026-08-29.

## Knowledge systems — use both

| System | Role | Location / access |
| --- | --- | --- |
| **agentcairn** | Durable **memory** — distilled decisions, rationale, how-we-work facts, for cross-session recall | vault `~/agentcairn`; `/agentcairn:*` commands; `mcp__plugin_agentcairn_agentcairn__*` tools (`recall`, `search`, `remember`, `recent`, `build_context`) |
| **ClaudeVault** | **Design / implementation / reference docs** — full write-ups with context, alternatives, diagrams | `~/Documents/ClaudeVault` (schema `claude-obsidian.workspace.v1`); project docs in `projects/microvm-controller/`; raw sources in `inbox/` |

**Workflow**

- **Before** starting non-trivial work: `recall` / `search` agentcairn (it also surfaces
  relevant memories automatically at prompt time).
- **After** a decision or fix: `remember` a short distilled note in agentcairn **and**
  write the full version into `~/Documents/ClaudeVault/projects/microvm-controller/`.
  The agentcairn note points back to the ClaudeVault path.
- Raw research / source material → `~/Documents/ClaudeVault/inbox/`.
- The two vaults are **separate stores and do not sync**. Do not use the empty vanilla
  Obsidian vault at `~/Dev/Claude`.
- Obsidian watches ClaudeVault live; it sometimes drops 0-byte stray notes at the vault
  root from new wikilinks — clean those up.

**Log every gotcha**

When a footgun costs real debugging time — a wrong kernel arg, an SDK surprise, an
upstream layout change, an environment limit — add it to
`~/Documents/ClaudeVault/projects/microvm-controller/Gotchas.md` **before moving on**.
One entry per gotcha, numbered `G-N`, in this shape:

- **Symptom:** the exact error text or observable behaviour
- **Cause:** the underlying reason, once known
- **Fix:** the concrete change, with the file it lives in
- **Applies to:** which phases / hosts

Also mirror a one-line pointer into an agentcairn `remember` note, and reference the
`G-N` id from the code comment or the [[Build Log]] entry that came out of it.

## Code style

All code comments and all log / error strings follow **ASD-STE100** (Simplified
Technical English). Run `/asd-ste100` when writing or reviewing them. Enforce the
structural rules: active voice, one instruction per sentence, ≤ 20 words for
instructions and ≤ 25 for descriptions, no semicolons, no phrasal verbs
("start" not "spin up", "remove" not "take off"), simple tenses, no nominalisation.
Keep one verb for one action across the codebase (this code uses "start" / "stop"
for VM lifecycle, never "boot" / "tear down").

## Canonical docs

- `~/Documents/ClaudeVault/projects/microvm-controller/MicroVM Controller.md` — map of content
- `~/Documents/ClaudeVault/projects/microvm-controller/Implementation Plan.md` — phased plan, target architecture, engineering decisions
- `~/Documents/ClaudeVault/projects/microvm-controller/Design Decisions.md` — ADR-style log `DD-01`..`DD-12` (each with alternatives rejected)
- `~/Documents/ClaudeVault/projects/microvm-controller/Build Log.md` — what got built per phase
- `~/Documents/ClaudeVault/projects/microvm-controller/Gotchas.md` — footguns, `G-N`, symptom → cause → fix
- `docs/implementation-plan.html` (this repo) — rendered plan; published artifact:
  https://claude.ai/code/artifact/9ae3fa52-4ebd-4290-b665-8e14dfc9dd84

## Architecture at a glance (see Design Decisions for rationale)

- **Isolation:** Firecracker + Jailer on KVM. One microVM per task session, never reused.
- **Control plane:** custom **Go** daemon on `firecracker-microvm/firecracker-go-sdk`;
  raw Unix-socket API for snapshot-load + `userfaultfd`. Not `firecracker-containerd` / Kata.
- **Guest agent:** static binary as PID 1, gRPC/protobuf over `AF_VSOCK` — `ExecStream`
  (spawn + PTY + streamed I/O + exit), `WriteFile`/`ReadFile`, `HealthCheck`; also child
  reaping, signal forwarding, post-resume clock resync.
- **Cold start:** build template once → snapshot → restore via per-VM `userfaultfd`
  handler; warm pool of paused VMs. **Deferred to Phase 4** — fresh-boot path first.
- **Networking:** one netns per VM + TAP + `/30` IPAM; `nftables` drops inter-VM traffic,
  `169.254.169.254`/link-local, loopback, RFC-1918 host LAN; egress through a host
  forward proxy (domain allow/blocklist, rate limits).
- **Limits:** cgroup v2 `cpu.max` / `memory.max` / `pids.max` on the VMM process
  (delegated systemd subtree, no root); Firecracker per-VM token-bucket rate
  limiters on the rootfs and the network interface.
- **Storage:** shared read-only `rootfs.ext4` + per-VM ephemeral CoW scratch disk;
  files move over vsock only (no virtio-fs / NFS).
- **Image pipeline:** `Dockerfile` → `docker export` → mounted `mkfs.ext4` → inject agent.
  Kernel: uncompressed `vmlinux` (LTS 6.1/6.6), `CONFIG_MODULES=n`.
- **Host:** bare metal for prod (Equinix / Hetzner / AWS `*.metal`); GCP nested virt for dev.
- **GPU:** out of scope for Firecracker (no VFIO). If needed later, a parallel gVisor /
  Cloud Hypervisor backend — keep the VM-driver interface abstract.

**Roadmap:** P1 minimal microVM + vsock echo ✅ · P2 exec + file I/O guest agent ✅ ·
P3 netns/TAP/nftables + cgroups v2 + rate limiters ✅ · P4 snapshot + UFFD + warm
pool + orphan reaper ✅ · P5 northbound HTTP API + auth/quotas + Prometheus metrics
+ egress proxy ✅.

**Status:** P1 + P2 + P3 done.
- P1: `microvmctl demo` starts a microVM; the static agent runs as PID 1; host↔guest
  vsock works.
- P2: `agent.v1.Agent` gRPC service over vsock (`proto/agent/v1/agent.proto`,
  generated into `gen/`). `Exec` (streamed stdout/stderr, exit code, PTY, timeout →
  kill group), `WriteFile` / `ReadFile` (sha256, byte-exact), `Info`. Host client:
  `internal/agentclient`. CLI: `microvmctl run [flags] -- CMD` with `-env`, `-tty`,
  `-timeout`, `-put`, `-get`. Integration test: `make test-integration`.
- P3 networking (`internal/netconf`): per-VM network namespace + veth + TAP + `/30`
  IPAM + `nftables` isolation rules (drop → host / other VMs / `169.254.0.0/16`,
  masquerade for egress). `microvmctl run -net [-uplink auto|none|<iface>]
  [-net-pool CIDR]`. `Apply`/`Destroy` need CAP_NET_ADMIN + CAP_SYS_ADMIN — real
  root, or `unshare --user --map-root-user --mount --net`. Test: `make test-net`
  (runs rootless in a user+net namespace).
- P3 limits (`internal/cgroup`): cgroup v2 `cpu.max` / `memory.max` / `pids.max` on
  the Firecracker process. Writes into this process's delegated systemd subtree
  (`…/user@<uid>.service/microvm/mvm-<id>`), so it needs no root when systemd
  delegation is in effect. `cgroup.Available()` preflights; `Instance.Stop` removes
  the cgroup after the VMM exits. Firecracker per-VM rate limiters cap rootfs and
  network bandwidth (`models.RateLimiter`, one-second token bucket). CLI:
  `microvmctl run -cpu 0.5 -mem-max 256 -pids 128 -disk-bw BYTES -net-bw BYTES`.
  Daemon: `microvmd -cpu-max -mem-max -pids-max -disk-bw -net-bw` per template.
  Test: `make test-integration` (`internal/vm` `TestBootWithCgroupLimits`,
  `internal/cgroup` unit tests).
- P4 (`internal/vm` snapshot + `internal/pool`): full snapshot/restore
  (`microvmctl snapshot` / `restore`) — restore reaches a ready agent in ~25 ms vs
  ~1.1 s cold. Warm pool of running microVMs (`internal/pool`, `microvmctl bench`)
  — p50 acquire ~1 ms; leases carry a TTL and idle timeout and a sweeper
  force-releases stragglers (`bench -lease-ttl` / `-lease-idle`, `Lease.Touch`).
  Orphan reaper (`vm.Reconcile`, `microvmctl reap`) kills Firecracker processes a
  crash left behind. `cmd/microvm-uffd` is a working userfaultfd page-fault
  handler; `microvmctl restore -uffd` streams guest pages from `mem.bin` on
  demand via a raw `snapshot/load` with `mem_backend`. Tests: `make test-integration`.
- UFFD restore needs read access to `/dev/userfaultfd` or
  `vm.unprivileged_userfaultfd=1` — real root on this box (Gotcha `G-14`). The
  handler's wire protocol is unit-tested rootless; the end-to-end restore test
  skips without the privilege.
- Diff snapshots (`internal/vm/diff.go`): `RestoreForDiff` restores a full
  snapshot with dirty-page tracking on (`SnapshotConfig.EnableDiffSnapshots`);
  `Instance.CreateSnapshotDiff` then writes a sparse `mem.bin` with only the
  changed pages plus a full `snapshot.bin`. `RestoreDiff` rebases the sparse
  diff onto a copy of the base memory (`RebaseMemory`, `SEEK_DATA`/`SEEK_HOLE`)
  and restores from the merge. A diff snapshot must come from a fresh restore,
  never a resumed instance (Gotcha `G-12`). CLI: `microvmctl snapshot -exec
  ... -diff-exec ...` writes `<dir>` + `<dir>/diff`; `microvmctl restore -diff`.
  On this box a 256 MiB guest's diff `mem.bin` is ~2 MiB on disk.
- Still todo for P4: a snapshot-templated pool.
- P5 (`cmd/microvmd`, `internal/server`, `internal/egress`): HTTP/JSON API —
  `POST/GET/DELETE /v1/sandboxes`, streaming `POST /v1/sandboxes/{id}/exec`
  (NDJSON), `PUT/GET /v1/sandboxes/{id}/files/{path}`. Bearer-token auth, per-token
  + global concurrency quotas, multiple templates, TTL janitor, warm-pool option,
  startup orphan reconcile. `GET /metrics` (Prometheus, `microvmd_*`), structured
  JSON request logs. `internal/egress` is a CONNECT/HTTP forward proxy with a
  domain allow/deny policy; networked sandboxes get `HTTP(S)_PROXY` pointed at it.
  Run: `make serve` or `MICROVMD_TOKENS=id:secret microvmd -pool 4`. API reference:
  `docs/api.md`. Tests: `make test-integration` (`internal/server`) +
  `internal/egress` unit tests.
- `microvmd -config FILE` (JSON) defines the templates, the tokens, and a
  per-token policy: `templates` allowlist, `max_concurrent`, `max_ttl`,
  `deny_network`. Enforced in `handleCreate` before any boot — a blocked
  request gets `403`. `server.Config.Policies map[string]TokenPolicy`. Sample:
  `docs/microvmd.example.json`. Loader + validation: `cmd/microvmd/config.go`.
- Transparent egress (`internal/egress/transparent.go`): `microvmd
  -egress-transparent ADDR` starts a second listener that takes raw
  connections. An nftables `prerouting` DNAT in each sandbox namespace
  (`Plan.RedirectPort`) forces guest TCP 80/443 to it, so the guest cannot
  bypass the proxy by ignoring `HTTP(S)_PROXY`. The proxy recovers the pre-DNAT
  address with `SO_ORIGINAL_DST`, reads the host from the HTTP request line or
  the TLS SNI (no TLS termination), applies the same `Policy`, and splices. SNI
  and Host parsing are unit-tested; the DNAT rule needs a real root host (same
  as the rest of `netconf`, Gotcha `G-11`).
- Still todo for P5: a gRPC surface alongside REST.
- Pool members stay **running**, not paused: on this Firecracker build the guest
  vsock stops answering after `PauseVM`/`ResumeVM` on the same process (Gotcha
  `G-12`). Snapshot restore (a fresh process) is unaffected.
- The jailer path (`-jail`) and real-uplink NAT (`-uplink <iface>`) are coded but
  need a real root host to exercise.

### Protobuf

`gen/` is committed. Regenerate with `make proto` (needs `make tools` once: installs
`buf`, `protoc-gen-go`, `protoc-gen-go-grpc` into `$GOBIN` — no root). `buf` is a
single Go binary; there is no system `protoc`.

## Tooling notes

- Research uses Antigravity CLI: `~/.local/bin/agy -p "..."` (non-interactive).
- `uv`/`uvx` installed via `pipx install uv` (0.12.7) — required for the agentcairn MCP
  server (`command: uvx`).
