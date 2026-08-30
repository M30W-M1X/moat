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

## Canonical docs

- `~/Documents/ClaudeVault/projects/microvm-controller/MicroVM Controller.md` — map of content
- `~/Documents/ClaudeVault/projects/microvm-controller/Implementation Plan.md` — phased plan, target architecture, engineering decisions
- `~/Documents/ClaudeVault/projects/microvm-controller/Design Decisions.md` — ADR-style log `DD-01`..`DD-12` (each with alternatives rejected)
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
- **Storage:** shared read-only `rootfs.ext4` + per-VM ephemeral CoW scratch disk;
  files move over vsock only (no virtio-fs / NFS).
- **Image pipeline:** `Dockerfile` → `docker export` → mounted `mkfs.ext4` → inject agent.
  Kernel: uncompressed `vmlinux` (LTS 6.1/6.6), `CONFIG_MODULES=n`.
- **Host:** bare metal for prod (Equinix / Hetzner / AWS `*.metal`); GCP nested virt for dev.
- **GPU:** out of scope for Firecracker (no VFIO). If needed later, a parallel gVisor /
  Cloud Hypervisor backend — keep the VM-driver interface abstract.

**Roadmap:** P1 minimal jailed microVM + vsock echo · P2 exec + file I/O guest agent ·
P3 netns/TAP/nftables + cgroups · P4 snapshot/UFFD + warm pool + orphan reaper ·
P5 public gRPC/REST gateway + Prometheus metrics + egress guardrail proxy.

## Tooling notes

- Research uses Antigravity CLI: `~/.local/bin/agy -p "..."` (non-interactive).
- `uv`/`uvx` installed via `pipx install uv` (0.12.7) — required for the agentcairn MCP
  server (`command: uvx`).
