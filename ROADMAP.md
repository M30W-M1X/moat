# Moat Roadmap

Everything that isolates a single host ships today under the **MIT License** ([`LICENSE`](LICENSE)). The security boundary is never paywalled.

---

## Shipped

### v0 — Isolation Engine
- Firecracker + KVM microVM lifecycle with `jailer` confinement (chroot, seccomp-BPF, dropped privileges).
- Static Go PID 1 guest agent (`agent.v1.Agent` over `AF_VSOCK`): streamed exec with PTY & timeouts, byte-exact file transfer, orphan zombie reaper, overlayfs root pivot, and wall-clock resync.
- Per-VM network namespaces, TAP + veth + `/30` IPAM (released on destroy), and strict `policy drop` `nftables` blocking the host, `169.254.169.254`, loopback, and RFC-1918 ranges.
- Forward and transparent SNI/Host egress proxy with global + per-token allow/deny lists (global deny always wins).
- Shared read-only rootfs paired with sparse per-VM ext4 overlayfs scratch disks (`read_only_root`) or private sparse/reflink copies.
- cgroup v2 `cpu.max` / `memory.max` / `pids.max` caps and Firecracker disk/NIC token-bucket rate limiters.
- Full and diff snapshots, `moat-uffd` userfaultfd demand-paging handler, per-template warm pools (cold or snapshot-backed), TTL/idle janitor, and startup orphan reconciliation.

### M1 — Self-Serve Agent Sandbox
- Ingress secret firewall (vendored Gitleaks rules; `409` on hit, audited override, optional `-secret-redact`).
- `moat-mcp` Model Context Protocol server over stdio (Unix socket or HTTP/HTTPS) and `SKILL.md` agent playbook.
- One-command installer (`install.sh` with system and `--user` modes), `moat doctor` preflight, `moat template import` for any `linux/amd64` OCI image, and `moat uninstall`.

### M2 — Audit, Trust & Single-Node Production Hardening
- Append-only, hash-chained, metadata-only JSONL audit log, `moat audit {tail,query,report,verify}`, `GET /v1/sandboxes/{id}/report`, and async webhook SIEM forwarding (`audit.webhook_url`).
- Deterministic, byte-reproducible rootfs builds (`make images-verify` in CI), CycloneDX SBOM (`sbom.cdx.json`), automated Cosign-signed releases, and `images.lock` digest + signature enforcement at `moatd` startup.
- Native TLS 1.2+ / mTLS (`tls_cert`, `tls_key`, `tls_client_ca`), `0660` Unix domain socket listener (`socket`), and bounded HTTP request sizes/timeouts.

---

## Next Up (Single-Node & API Polish)

Items carried forward from the original architecture plan:

- **Northbound gRPC API & runtime snapshot route:** gRPC surface alongside the REST API, plus `POST /v1/sandboxes/{id}/snapshot` for checkpointing live sandboxes via the API.
- **High-density memory & I/O controls:** `virtio-balloon` memory reclaim for idle sandboxes, cgroup v2 `io.max` per VMM, and per-sandbox vCPU/RSS Prometheus metrics.
- **Outbound egress DLP & rate limiting:** optional secret scanning on outbound HTTP proxy payloads and per-sandbox egress request rate limits.
- **UFFD hardening & host tuning:** continuous fuzzing of the `moat-uffd` page-fault handler path and automated `moat doctor` checks for `fs.file-max` / inotify limits at high VM density.
- **Optional Rust guest agent:** drop-in `<3 MiB` static Rust PID 1 agent speaking the same `agent.v1.Agent` protobuf schema for minimal footprint.

---

## M3 — Multi-Node Fleet & Platform Reach

- **Multi-node fleet control plane (open-core):** scheduling across bare-metal hosts (Equinix Metal, Hetzner, AWS `*.metal`), per-team RBAC/SSO, web console, and managed hosting. Single-node operation and every isolation/audit guarantee remain MIT forever.
- **GPU workloads:** parallel VMM driver backend (Cloud Hypervisor with VFIO or gVisor) while keeping the daemon's driver interface abstract (Firecracker has no PCI passthrough).
- **Broader platform & ecosystem reach:**
  - `arm64` Linux support
  - Downloadable per-language template registry (`moat template pull`)
  - Official Python, TypeScript, and Go client SDKs
  - macOS Linux-helper VM and WSL2 nested-virtualization setup guides
