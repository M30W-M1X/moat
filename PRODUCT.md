# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Stack

The product is Go (daemon `moatd`, CLI `moat`, MCP server `moat-mcp`, guest
agent). The marketing site is hand-authored standalone HTML + CSS with a little
vanilla JS, no framework, served from `docs/` for GitHub Pages
(`docs/index.html`). Fonts from Google Fonts; everything else inline.

## Users

Primary: **individual developers who let an AI coding agent run commands on
their own machine** — Claude Code, Cursor, Codex, Aider and similar — and are
uneasy about what a generated command, a package install, or a prompt injection
could do to their host.

All three of these audiences are real and matter:

1. **Solo developers** running an AI coding agent on their own machine — the
   lead story and the widest funnel.
2. **Engineers building agent products** who need a per-task sandbox on their
   own infrastructure.
3. **Security-conscious organizations** that cannot send code or data to a
   third-party sandbox.

They all install the same way, so the marketing leads with the solo-dev case
and lets the other two follow from it.

## Product Purpose

Moat is an open-source sandbox that gives an AI agent a disposable,
hardware-isolated Linux microVM to run commands in. Each sandbox is a real VM on
KVM (Firecracker), with its own kernel, no access to the host filesystem,
network, or credentials, and is destroyed after the task. It installs on the
user's own Linux machine with one command and plugs into any MCP-compatible
agent.

Success: a developer can let an agent run installs, generated code, and network
fetches without those actions being able to reach anything of theirs — and the
setup took one command, with nothing sent anywhere.

## Positioning

A real virtual machine per task, on the user's own hardware, driven by an MCP
agent, with a credential firewall on the way in. Specifically:

- **Hardware isolation, not namespaces.** A separate guest kernel under KVM —
  the isolation model AWS uses for Lambda tenants — not a shared-kernel
  container. Container-escape-class kernel bugs do not apply.
- **Self-hosted.** Runs entirely on the user's machine. Code and data never
  leave it. No telemetry. Contrast with hosted third-party sandboxes.
- **Ephemeral by default.** One microVM per task, never reused, destroyed on
  exit, TTL, or idle. Restore-from-snapshot reaches a ready sandbox in tens of
  milliseconds, so it is cheap enough to use per command.
- **Ingress secret firewall.** Every file, environment value, and stdin
  entering a sandbox is scanned for credentials and blocked by default —
  nothing of the user's leaks in, not just out.

## Operating Context

- Host: bare-metal or nested-virt Linux, x86_64, with `/dev/kvm`. Firecracker
  and jailer on `PATH`.
- Components: `moatd` (HTTP/JSON API daemon, usually a systemd unit), `moat`
  (control CLI: `run`, `snapshot`/`restore`, `doctor`, `template`), `moat-mcp`
  (Model Context Protocol server over stdio), and a static guest agent that
  runs as PID 1 in the microVM and speaks gRPC over vsock.
- The agent drives it through `moat-mcp`: `moat_create` → `moat_write_file` →
  `moat_exec` → `moat_read_file` → `moat_destroy`. `SKILL.md` is the agent
  playbook. There is also a plain HTTP API and the CLI.
- Install: the `install.sh` asset from a GitHub release, run against the
  release tarball. `https://moat.sh/install` is a planned alias — `moat.sh` is
  not registered — so use the GitHub URLs (`github.com/M30W-M1X/moat`) as the
  canonical source until a domain exists. Config in `moatd.json` — templates,
  bearer tokens,
  per-token policy (allowed templates, concurrency, TTL, deny-network,
  secret-override).
- Networked sandboxes get a per-VM network namespace and nftables rules that
  drop routes to the host, cloud metadata, and RFC-1918; egress only through a
  domain-filtered proxy that a transparent redirect makes unbypassable.

## Capabilities and Constraints

Shipped (engine P1–P5, product milestone M1, and single-node production hardening M2):

- microVM lifecycle with the jailer; read-only rootfs + ephemeral ext4 overlayfs
  scratch disks; snapshot/restore (with automatic guest clock resync) and diff
  snapshots; per-template cold or snapshot-backed warm pools; an orphan reaper.
- Guest agent as PID 1: zombie reaper, streamed exec with PTY and timeouts,
  byte-exact file transfer over vsock.
- Limits: cgroup v2 `cpu.max` / `memory.max` / `pids.max`; Firecracker
  token-bucket rate limiters on the disk and NIC.
- Northbound HTTP/JSON API: native TLS 1.2+/mTLS and Unix domain socket
  listeners, bearer-token auth, per-token template/quota/egress policy, TTL
  janitor, Prometheus metrics, structured logs.
- Egress domain-filter proxy (global + per-token policy) plus a transparent
  nftables redirect and strict `policy drop` forward chain.
- Ingress secret firewall (vendored Gitleaks rule set; `409` on a hit, audited
  override, optional redaction).
- Hash-chained, metadata-only JSONL audit log, `moat audit` CLI, per-sandbox
  activity report API, and async webhook SIEM sink.
- Reproducible image builds (`make images-verify` in CI), CycloneDX SBOM,
  `images.lock` digest + Cosign signature verification enforced at startup, and
  `moat template import` for any linux/amd64 OCI image.
- `moat-mcp` MCP server (HTTP(S) and Unix socket), `SKILL.md`, `moat doctor`,
  `moat uninstall`.
- `install.sh` one-command installer; automated Cosign-signed release workflow;
  a hardened `deploy/moatd.service` systemd unit.

Constraints:

- Linux + KVM + x86_64 only. No arm64 yet.
- No GPU — Firecracker has no device passthrough. GPU support would need a
  different VMM backend.
- macOS and Windows only as hosts of a Linux VM (WSL2 with nested virt on
  Windows).
- Single-node. A multi-node fleet control plane is milestone M3, not shipped.
- No gRPC northbound surface yet; no downloadable per-language template registry
  (`moat template pull` — covered locally by `moat template import`).
- userfaultfd restore needs root or `vm.unprivileged_userfaultfd=1`.
- The daemon needs `/dev/kvm` and a delegated cgroup v2 subtree; networked
  sandboxes and the jailer need `CAP_NET_ADMIN` / `CAP_SYS_ADMIN` or real root.

Terminology: *sandbox* = one microVM handed to an API caller. *template* =
a kernel + rootfs a sandbox starts from. *the ingress secret firewall* screens
bytes crossing *into* a sandbox; the *egress proxy* filters traffic going *out*.

## Brand Commitments

- **Name:** Moat. Daemon `moatd`, CLI `moat`, MCP server `moat-mcp`. Repo and
  module path `github.com/M30W-M1X/moat`.
- **Tagline:** "A moat around your agent." **Hero line:** "Give your agent
  root. Burn the VM." (both confirmed by the user; the hero payoff was chosen
  over "Keep your machine." in a copy review).
- **Voice:** dry, confident, technical, a little edge. No exclamation marks, no
  "unleash / supercharge / seamless / effortless", no startup-cheerful. A
  control says exactly what it does.
- **License:** MIT, open-core. Free forever: the daemon, CLI, guest
  agent, MCP server, `SKILL.md`, single-node operation, and every isolation
  feature. A later paid tier covers multi-node fleet, a web console, SSO/RBAC,
  and managed hosting. **The security boundary is never in the paid tier** —
  state this whenever licensing comes up.
- **Legal:** independent open-source project. Not affiliated with, or endorsed
  by, Amazon Web Services. "Firecracker" is a trademark of Amazon.com, Inc.
  Keep this disclaimer on any public page.
- A visual identity already exists in `docs/index.html` (recorded separately, not
  here).

## Evidence on Hand

- **Working software.** Engine phases P1–P5 and milestones M1–M2 are built and on
  `main`. Tagged release `v1.0.0` at `github.com/M30W-M1X/moat/releases/tag/v1.0.0`
  (binaries + images + `checksums.txt` + Cosign signatures). Public repo and GitHub
  Pages site at `https://m30w-m1x.github.io/moat/`.
- **Benchmarks**, measured on one developer machine (AMD Ryzen 7 5700G, Linux
  7.2, KVM, x86_64, Firecracker v1.16.1, 256 MiB Alpine guest) — not
  guarantees: Firecracker VMM cold start ~20 ms; snapshot restore to ready agent
  with clock resync ~35 ms; warm-pool acquire p50 <1 ms (444–755 µs over 25
  cycles); a 256 MiB guest's diff memory image ~4.4 MiB on disk (3 ms write).
  Always label these as indicative, single-machine measurements.
- **Docs:** `README.md`, `docs/api.md`, `docs/mcp.md`, `docs/install.md`,
  `SKILL.md`, `docs/moatd.example.json`; the marketing page `docs/index.html`;
  the fuller plan in `~/Documents/ClaudeVault/projects/microvm-controller/`.
- **No** customers, testimonials, case studies, press, adoption numbers,
  pricing, or a hosted offering exist yet. Future work must not invent any.

## Product Principles

1. **The security boundary is never paywalled.** Isolation is the product;
   convenience for fleets is what a paid tier may sell.
2. **Minimal trusted surface.** Resist dependencies in the daemon — the
   Gitleaks *rules* were vendored rather than importing the library and its
   200+ modules. Every added dependency is weighed against the isolation claim.
3. **Nothing leaves the machine.** Self-hosted, no telemetry, no account. The
   only outbound path is the egress proxy the user configures.
4. **Ephemeral, not persistent.** A sandbox is single-use and thrown away.
   State never carries between tasks.
5. **Built for an agent, not a person.** The primary interface is MCP tools and
   a `SKILL.md`, with a credential firewall on ingress and machine-readable
   results, because the caller is a model.

## Accessibility & Inclusion

No accessibility requirement. The user has explicitly deprioritised it — do not
spend effort on a compliance target or re-raise it. The good-practice basics
already in the marketing page (visible focus, `prefers-reduced-motion`,
theme-aware contrast, semantic structure) can stay, but nothing here is a
constraint on future work.
