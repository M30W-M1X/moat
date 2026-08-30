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

Secondary: engineers building agent products who need a per-task sandbox on
their own infrastructure; security-conscious organizations that cannot send
code or data to a third-party sandbox.

*(Inferred ordering: I asked which audience leads and the question was declined,
so this reflects the approved product plan — solo devs are the widest funnel and
the other two install the same way.)*

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
- Install: `curl -fsSL https://moat.sh/install | sh` (or the `install.sh` asset
  from a GitHub release). Config in `moatd.json` — templates, bearer tokens,
  per-token policy (allowed templates, concurrency, TTL, deny-network,
  secret-override).
- Networked sandboxes get a per-VM network namespace and nftables rules that
  drop routes to the host, cloud metadata, and RFC-1918; egress only through a
  domain-filtered proxy that a transparent redirect makes unbypassable.

## Capabilities and Constraints

Shipped (engine P1–P5 and product milestone M1):

- microVM lifecycle with the jailer; snapshot/restore and diff snapshots; a
  warm pool; an orphan reaper.
- Guest agent: streamed exec with PTY and timeouts, byte-exact file transfer,
  over vsock.
- Limits: cgroup v2 `cpu.max` / `memory.max` / `pids.max`; Firecracker
  token-bucket rate limiters on the disk and NIC.
- Northbound HTTP/JSON API: bearer-token auth, per-token template and quota
  policy, TTL janitor, Prometheus metrics, structured logs.
- Egress domain-filter proxy plus a transparent nftables redirect.
- Ingress secret firewall (vendored Gitleaks rule set; `409` on a hit, audited
  override, optional redaction).
- `moat-mcp` MCP server, `SKILL.md`, `moat doctor`.
- `install.sh` one-command installer; `make dist` release build; a
  `deploy/moatd.service` systemd unit.

Constraints:

- Linux + KVM + x86_64 only. No arm64 yet.
- No GPU — Firecracker has no device passthrough. GPU support would need a
  different VMM backend.
- macOS and Windows only as hosts of a Linux VM (WSL2 with nested virt on
  Windows).
- Single-node. A multi-node fleet control plane is milestone M2, not shipped.
- No gRPC surface yet; no snapshot-templated pool; no downloadable per-language
  template registry (`moat template pull`).
- userfaultfd restore needs root or `vm.unprivileged_userfaultfd=1`.
- The daemon needs `/dev/kvm` and a delegated cgroup v2 subtree; networked
  sandboxes need `CAP_NET_ADMIN` / `CAP_SYS_ADMIN` or real root.

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
- **License:** Apache-2.0, open-core. Free forever: the daemon, CLI, guest
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

- **Working software.** Engine phases P1–P5 and milestone M1 are built and on
  `main`. Tagged release `v0.1.0` at `github.com/M30W-M1X/moat/releases/tag/v0.1.0`
  (binaries + images + `checksums.txt` + `install.sh`).
- **Benchmarks**, measured on one developer machine (Fedora, KVM, x86_64) — not
  guarantees: cold boot to a ready in-VM agent ~24 ms; snapshot restore ~25 ms;
  warm-pool acquire p50 ~1 ms; a 256 MiB guest's diff memory image ~2 MiB on
  disk. Always label these as indicative, single-machine measurements.
- **Docs:** `README.md`, `docs/api.md`, `docs/mcp.md`, `docs/install.md`,
  `SKILL.md`, `docs/moatd.example.json`; the marketing page `docs/index.html`;
  the fuller plan in `~/Documents/ClaudeVault/projects/microvm-controller/`.
- **No** customers, testimonials, case studies, press, adoption numbers,
  pricing, or a hosted offering exist yet. Future work must not invent any.
- `moat.sh` is referenced as the canonical install host; whether the domain is
  registered is unconfirmed (the question was declined). Until confirmed, treat
  GitHub release URLs as the fallback of record.

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

No formal standard has been set. *(Inferred: the accessibility question was
declined.)* The marketing site already carries the good-practice basics —
visible keyboard focus, `prefers-reduced-motion`, theme-aware contrast in light
and dark, semantic structure, horizontal-scroll containment — and future web
work should keep them. Adopt a named target (e.g. WCAG 2.2 AA) only when the
user sets one.
