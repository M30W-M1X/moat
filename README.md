# Moat

**Give your agent root. Burn the VM.**

Moat is an open-source, self-hosted sandbox for AI coding agents and untrusted code. Every task runs inside its own disposable Firecracker microVM on KVM — separate Linux kernel, no host filesystem, no host network, no host credentials — then disappears the moment the task ends.

No containers sharing your kernel. No third-party cloud holding your source code. One command on your Linux machine.

---

## Why Moat

Coding agents install packages, execute generated scripts, and fetch from the internet on their own. Where that command runs decides what one bad tool call — or one prompt injection — can reach.

| Where the command runs | Kernel boundary | Host files & credentials | Your code stays local | Cost per task |
| --- | --- | --- | --- | --- |
| **Directly on your host** | None | Full access (`~/.ssh`, `~/.aws`, browser cookies) | Yes | Free until it burns you |
| **Docker / container** | Shared host kernel | Exposed by container escapes or leaked mounts | Yes | Low |
| **Hosted cloud sandbox** | Provider VM | Isolated from laptop, sent to third-party servers | **No** | Per-minute bill + latency |
| **Moat (Firecracker + KVM)** | **Dedicated guest kernel per task** | **None mounted; ingress secret firewall** | **Yes — runs on your metal** | **~1 ms from a warm pool** |

---

## 60-second quickstart

Requires Linux `x86_64` with `/dev/kvm`.

```sh
# Install binaries, verified kernel + rootfs, starter config, and systemd unit
curl -fsSL https://raw.githubusercontent.com/M30W-M1X/moat/main/install.sh | sh

# Verify KVM, cgroup v2 delegation, Firecracker, and image digests
moat doctor

# Start the daemon
systemctl enable --now moatd

# Run your first command inside a throwaway microVM
moat run -- uname -a
```

Prefer a rootless install under `$HOME` (`~/.local/bin`, `~/.local/share/moat`, `~/.config/moat`)? Append `-s -- --user`.

### Connect your AI agent (MCP)

Drop one block into Claude Code, Claude Desktop, Cursor, or any MCP client (using the token generated in `/etc/moat/moatd.json` or `~/.config/moat/moatd.json`):

```json
{
  "mcpServers": {
    "moat": {
      "command": "moat-mcp",
      "args": ["--socket", "/run/moat/moatd.sock"],
      "env": { "MOAT_TOKEN": "dev:<secret>" }
    }
  }
}
```

Give the agent [`SKILL.md`](SKILL.md) so it knows when to isolate commands, how to handle a secret-firewall block without overriding it, and to default to `network: "deny"`. See [`docs/mcp.md`](docs/mcp.md).

---

## Every threat, and the control that stops it

Every control below ships in the Apache-2.0 core. Nothing in the security boundary is paywalled.

| If the command tries to... | Moat's control |
| --- | --- |
| **Read or modify host files** | No host filesystem is mounted. Every sandbox gets a private sparse copy or a shared read-only rootfs paired with an ephemeral ext4 overlayfs scratch disk (`read_only_root`). |
| **Steal host credentials** | Host env is deny-by-default. The ingress secret firewall scans every file, env var, and stdin crossing in against the vendored Gitleaks rule set and returns `409` on a hit (with audited override or `-secret-redact`). |
| **Pivot to your LAN or cloud metadata** | Per-VM network namespace with `nftables` rules dropping routes to the host, `169.254.169.254`, loopback, and RFC-1918 ranges. |
| **Exfiltrate data over the network** | Outbound HTTP/HTTPS flows through a domain-filtered proxy with global and per-token allow/deny lists. Transparent DNAT (`-egress-transparent`) plus `strict_egress` (`policy drop` on forward) prevents guests from bypassing the proxy. |
| **Fork-bomb, OOM, or thrash disk** | cgroup v2 `cpu.max`, `memory.max`, and `pids.max` on the VMM, plus Firecracker token-bucket rate limiters on disk and NIC. |
| **Exploit a container-escape kernel bug** | Separate Linux guest kernel under KVM, optionally confined under the Firecracker `jailer` (chroot, seccomp filter, dropped privileges). |
| **Leave state behind for the next task** | One microVM per task, never reused. Destroyed on exit, TTL, or idle timeout. An orphan reaper sweeps anything a crash leaves behind. |
| **Tamper with what happened** | Hash-chained, metadata-only JSONL audit log of every create, exec, file transfer, secret block, and egress verdict — verifiable with `moat audit verify` and streamable to a SIEM via webhook. |

---

## Performance

Fast enough to give every task its own virtual machine:

| Metric | Reading |
| --- | --- |
| Cold start to ready in-VM agent | ~24 ms |
| Snapshot restore to ready agent (with automatic clock resync) | ~25 ms |
| Warm-pool acquire (p50) | ~1 ms |
| Diff-snapshot memory image (256 MiB guest) | ~2 MiB on disk |

*Indicative measurements on one developer machine (Fedora, KVM, x86_64) — not guarantees. Numbers move with hardware and template size.*

---

## Built for solo developers, agent platforms, and security teams

### For developers running coding agents locally
- **Drop-in MCP server (`moat-mcp`):** `moat_create`, `moat_write_file`, `moat_exec`, `moat_read_file`, `moat_list`, `moat_destroy` over stdio, talking to `moatd` via Unix socket or HTTP(S).
- **One-shot CLI (`moat run`):** run untrusted scripts, package installs, or builds straight from your shell with `-put`, `-get`, `-tty`, `-timeout`, `-net`, and client-side secret scanning.
- **Bring any container image:** `moat template import python:3.12` turns any `linux/amd64` OCI image or tar archive into a microVM template, preserving `ENV`, `WORKDIR`, and `ENTRYPOINT`.

### For engineers building agent products
- **Clean HTTP/JSON API (`moatd`):** lifecycle, streamed NDJSON exec, binary file transfer with SHA-256 trailers, Prometheus `/metrics`, and structured JSON request logs. See [`docs/api.md`](docs/api.md).
- **Per-template warm pools & snapshots:** keep warm microVMs ready per template, backed by cold starts or snapshots (`snapshot_dir`, optional `use_uffd` demand-paging) with automatic guest clock resync.
- **Multi-tenant policy per bearer token:** restrict allowed templates, max concurrency, max TTL, networking, per-token egress domain allow/deny lists, secret override rights, and audit read scope.

### For security-conscious organizations
- **Nothing leaves your infrastructure:** self-hosted, zero telemetry, zero external accounts.
- **Transport hardening:** native TLS 1.2+, mutual TLS (`tls_client_ca`), Unix domain socket mode (`socket` with `0660` permissions), and bounded request bodies/timeouts.
- **Tamper-evident audit trail:** hash-chained JSONL logs (`moat audit tail|query|report|verify`), per-sandbox activity rollups (`GET /v1/sandboxes/{id}/report`), and an async non-blocking webhook sink (`audit.webhook_url`) for SIEM ingestion.
- **Supply-chain verification:** deterministic, reproducible rootfs builds (`make images-verify`), CycloneDX SBOM (`sbom.cdx.json`), and `images.lock` digest + Cosign signature verification enforced by `moatd` at startup.

---

## Try it in five lines

### CLI
```sh
# Run a command with file I/O and a timeout
moat run -put ./data.csv:/work/data.csv -get /work/out.json:./out.json \
  -timeout 30s -- sh -c 'wc -l /work/data.csv > /work/out.json'

# Import a container image as a microVM template
moat template import python:3.12-slim --name python --config /etc/moat/moatd.json

# Inspect and verify the audit log
moat audit report sb-7f3a19c4
moat audit verify
```

### HTTP API
```sh
T='Authorization: Bearer s3cr3t'
ID=$(curl -s -XPOST -H "$T" -d '{"template":"python","ttl_seconds":120}' https://127.0.0.1:8443/v1/sandboxes | jq -r .id)
curl -s -XPUT  -H "$T" --data-binary @./task.py https://127.0.0.1:8443/v1/sandboxes/$ID/files/work/task.py
curl -s -XPOST -H "$T" -d '{"argv":["python3","/work/task.py"]}' https://127.0.0.1:8443/v1/sandboxes/$ID/exec
curl -s -XDELETE -H "$T" https://127.0.0.1:8443/v1/sandboxes/$ID
```

### End-to-end self-checking demo
```sh
make demo-image       # build the dev-shell rootfs (once)
make kernel           # fetch the pinned vmlinux (once)
./demo/run.sh         # proves kernel isolation, secret blocking, private disks, and audit chain
```

---

## Documentation

| Guide | What it covers |
| --- | --- |
| [`docs/install.md`](docs/install.md) | One-command install, `--user` mode, image verification, production hardening, `moat uninstall` |
| [`docs/mcp.md`](docs/mcp.md) | Wiring `moat-mcp` into Claude Code, Cursor, and other MCP clients |
| [`docs/api.md`](docs/api.md) | `moatd` HTTP API, `moatd.json` schema, secret firewall, egress policy, TLS/mTLS, audit API |
| [`docs/moatd.example.json`](docs/moatd.example.json) | Annotated production-ready daemon configuration |
| [`SKILL.md`](SKILL.md) | Agent playbook for ephemeral sandbox usage |
| [`demo/README.md`](demo/README.md) | Self-checking end-to-end isolation walk-through |
| [`build/templates/README.md`](build/templates/README.md) | Building custom templates and importing OCI images |
| [`docs/developer-guide.md`](docs/developer-guide.md) | Deep architecture, package map, security model, image pipeline, and operations |

---

## Build from source

Requirements: Linux `x86_64` with `/dev/kvm`, `firecracker` and `jailer` on `PATH` (tested with v1.16.1), Go 1.27+, `make`, `podman`, `mkfs.ext4`, and `curl`.

```sh
make all              # bin/{moat,moatd,moat-mcp,moat-uffd,guest-agent}
make images           # build/artifacts/{vmlinux,rootfs.ext4,sbom.cdx.json,images.lock}
make test             # unit tests
make test-integration # boots real microVMs (needs /dev/kvm)
make images-verify    # proves byte-identical rootfs rebuilds
```

<details>
<summary>Repository layout</summary>

| Path | Purpose |
| --- | --- |
| `cmd/moat` | Host CLI: `run`, `demo`, `snapshot`, `restore`, `bench`, `reap`, `doctor`, `template`, `audit`, `uninstall` |
| `cmd/moatd` | Northbound HTTP/JSON API daemon (TCP, TLS/mTLS, Unix socket) |
| `cmd/moat-mcp` | Model Context Protocol server over stdio |
| `cmd/moat-uffd` | Userfaultfd page-fault handler for on-demand snapshot restore |
| `guest/agent` | Static in-VM PID 1 agent (`agent.v1.Agent` over vsock, zombie reaper, overlayfs root, clock sync) |
| `internal/vm` | Firecracker & jailer driver, sparse/reflink rootfs copies, scratch ext4 builder |
| `internal/pool` | Per-template warm pools (cold or snapshot-backed) with lease TTL and idle reaping |
| `internal/netconf` | Per-VM netns, veth, TAP, `/30` IPAM, and strict `nftables` isolation |
| `internal/egress` | Forward and transparent SNI/Host egress proxy with global + per-token policy |
| `internal/secretscan` | Ingress credential firewall (vendored Gitleaks rules) |
| `internal/audit` | Append-only hash-chained JSONL audit log, reports, verification, and webhook sink |
| `internal/imagelock` | Reproducible image digest and Cosign signature verification |
| `internal/server` | HTTP API handlers, auth, quotas, TTL janitor, Prometheus metrics |
| `internal/moatapi` | Go client for `moatd` (HTTP, HTTPS, Unix socket) |

</details>

---

## License & notice

Licensed under **Apache-2.0**. The daemon, CLI, guest agent, MCP server, `SKILL.md`, single-node operation, and every isolation and audit control are free forever — **the security boundary is never paywalled**.

Moat is an independent open-source project. Not affiliated with, or endorsed by, Amazon Web Services. "Firecracker" is a trademark of Amazon.com, Inc.
