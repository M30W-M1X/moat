# Installing Moat

Go from a Linux box with KVM to hardware-isolated agent sandboxes in under two minutes. No account required. Nothing phones home.

## One-command install

### System install (recommended for servers and systemd)

```sh
curl -fsSL https://raw.githubusercontent.com/M30W-M1X/moat/main/install.sh | sh
```

### User install (no root required, installs under `$HOME`)

```sh
curl -fsSL https://raw.githubusercontent.com/M30W-M1X/moat/main/install.sh | sh -s -- --user
```

What the installer does:

1. Checks the host — Linux `x86_64` and `/dev/kvm`.
2. Downloads the release tarball and `checksums.txt` from `M30W-M1X/moat`.
3. Verifies the SHA-256 digest, and the Cosign signature when `cosign` is installed.
4. Installs `moat`, `moatd`, `moat-mcp`, and `moat-uffd` to `/usr/local/bin` (or `~/.local/bin`), and the guest agent, kernel, rootfs, `images.lock`, `images.lock.sig`, and CycloneDX SBOM to `/var/lib/moat` (or `~/.local/share/moat`).
5. Writes a starter `moatd.json` with a random bearer token if none exists.
6. Installs a hardened `moatd.service` systemd unit when systemd is present.

### Installer options

| Flag | Meaning |
| --- | --- |
| `--user` | Install under `$HOME` (`~/.local/bin`, `~/.local/share/moat`, `~/.config/moat`) |
| `--prefix DIR` | Override binary directory |
| `--data DIR` | Override image and audit directory |
| `--version VER` | Install a specific release tag (default: latest) |
| `--no-service` | Skip the systemd unit |
| `--dry-run` | Print the plan and change nothing |

Set `MOAT_REPO=owner/name` to install from a fork.

## Verify and run

```sh
moat doctor                       # checks KVM, Firecracker, cgroup v2, image digests
systemctl enable --now moatd      # or systemctl --user enable --now moatd
moat run -- echo hello from a microVM
```

Image lookup order: `$MOAT_DATA`, `$XDG_DATA_HOME/moat`, `~/.local/share/moat`, `/var/lib/moat`. A source checkout uses `build/artifacts`.

## Connect an AI agent

See [`docs/mcp.md`](mcp.md). The installer prints a ready-to-paste MCP config block using your generated token.

## Production hardening checklist

Out of the box, Moat isolates every task in its own KVM microVM with a private disk, ingress secret scanning, and hash-chained audit logging. For multi-tenant or unattended production hosts, enable these in `/etc/moat/moatd.json` (see [`docs/moatd.example.json`](moatd.example.json)):

1. **VMM jailer:** set `"defaults": { "jail": true }` (requires root or `CAP_SYS_ADMIN` + `CAP_NET_ADMIN` in `moatd.service`) so each Firecracker process runs chrooted with seccomp and dropped privileges.
2. **Read-only rootfs + overlayfs:** set `"read_only_root": true` and `"scratch_mib": 512` on templates to mount the shared ext4 image read-only and isolate writes onto a sparse per-VM scratch disk without copying the base image.
3. **Strict egress enforcement:** run `moatd` with `-egress-transparent 0.0.0.0:3128` and `"defaults": { "strict_egress": true }` so `nftables` drops all non-proxy outbound ports and transparently forces HTTP/HTTPS through the domain filter.
4. **Transport security:** bind to a local Unix socket (`"socket": "/run/moat/moatd.sock"` with `-addr ""`) for single-host agents, or set `"tls_cert"`, `"tls_key"`, and `"tls_client_ca"` for mTLS over TCP.
5. **Signed image verification:** keep `"image_verify": "enforce"` (the default) and set `"cosign_identity"` and `"cosign_issuer"` so `moatd` verifies `images.lock.sig` before serving any template.
6. **SIEM forwarding:** set `"audit": { "webhook_url": "https://..." }` to stream every tamper-evident audit event to your log collector.

## Verifying image provenance

Each release ships `images.lock`, `images.lock.sig`, and `sbom.cdx.json` beside `vmlinux` and `rootfs.ext4`. `images.lock` records the SHA-256 of each artifact plus the pinned base OCI digest, kernel CI snapshot, and SBOM digest.

Verify the local images by hand:

```sh
cd /var/lib/moat
python3 - <<'PY' | sha256sum -c -
import json
d = json.load(open("images.lock"))
for k in ("kernel", "rootfs", "sbom"):
    print(d[k]["sha256"], "", d[k]["path"])
PY
```

Rebuild from source and prove byte-for-byte reproducibility:

```sh
make images && make images-verify
```

On the same toolchain (`e2fsprogs`, `podman`, `go`, `tar`), two builds produce identical ext4 bytes. Across toolchains, the package set and CycloneDX SBOM still match.

## Building a signed release yourself

```sh
make images
SIGN=1 VERSION=v0.2.0 build/release.sh
```

`dist/` holds the tarball, `checksums.txt`, and Cosign signatures/bundles (`images.lock.sig` inside the tarball, `checksums.txt.sig` alongside it).

## Uninstall

Remove binaries, systemd unit, config, and data in one command:

```sh
moat uninstall                # system install (/usr/local/bin, /var/lib/moat, /etc/moat, unit)
moat uninstall --user         # user install (~/.local/bin, ~/.local/share/moat, ~/.config/moat)
moat uninstall --keep-data    # remove binaries and unit, keep images and audit logs
moat uninstall --dry-run      # print what would be removed without changing files
```
