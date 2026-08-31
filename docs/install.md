# Installing Moat

## One command

```sh
curl -fsSL https://moat.sh/install | sh
```

The installer:

1. checks the host — Linux, x86_64, and `/dev/kvm`
2. downloads the release tarball and `checksums.txt` for `M30W-M1X/moat`
3. verifies the sha256, and the cosign signature when `cosign` is present
4. installs `moat`, `moatd`, `moat-mcp`, `moat-uffd` to `/usr/local/bin` and the
   guest agent plus images to `/var/lib/moat`
5. writes a starter `/etc/moat/moatd.json` with a random token, if none exists
6. installs a `moatd` systemd unit when systemd is present

Run it as your user (`--user`) to install under `$HOME` instead:

```sh
curl -fsSL https://moat.sh/install | sh -s -- --user
```

### Options

| Flag | Meaning |
| --- | --- |
| `--user` | install under `$HOME` (`~/.local/bin`, `~/.local/share/moat`, `~/.config/moat`) |
| `--prefix DIR` | binary directory |
| `--data DIR` | image directory |
| `--version VER` | a specific release tag (default: latest) |
| `--no-service` | skip the systemd unit |
| `--dry-run` | print the plan, change nothing |

`MOAT_REPO=owner/name` points the installer at a fork.

## After install

```sh
moat doctor                       # KVM, firecracker, cgroup v2, images, ...
systemctl enable --now moatd      # or the printed run command
moat run -- echo hello from a microVM
```

Where the daemon and CLI look for the images, in order: `$MOAT_DATA`,
`$XDG_DATA_HOME/moat`, `~/.local/share/moat`, `/var/lib/moat`. A repo checkout
uses `build/artifacts` instead.

## Point an agent at it

See [`mcp.md`](mcp.md). The MCP config block, with the token from your
`moatd.json`, is also printed at the end of the install.

## Build a release yourself

```sh
make images            # kernel + rootfs (optional; bundled into the tarball if present)
VERSION=v0.1.0 make dist
```

`dist/` then holds `moat_<ver>_linux_amd64.tar.gz` and `checksums.txt`. Sign
`checksums.txt` with cosign and attach both, plus the signature, to a GitHub
release.

## Verifying the image

The release ships `images.lock` and `sbom.cdx.json` beside `vmlinux` and
`rootfs.ext4`. `images.lock` records the sha256 of each file plus the base
image digest, the kernel snapshot, and the SBOM digest.

Check the images by hand:

```sh
cd /var/lib/moat
python3 - <<'PY' | sha256sum -c -
import json
d = json.load(open("images.lock"))
for k in ("kernel", "rootfs", "sbom"):
    print(d[k]["sha256"], "", d[k]["path"])
PY
```

`moatd` runs the same check at startup. In the default `enforce` mode it does
not serve a template whose kernel or rootfs does not match `images.lock`. When
the release also ships `images.lock.sig`, set `cosign_identity` (or
`MOAT_COSIGN_IDENTITY`) and `moatd` verifies the signature too. `moat doctor`
prints an `image-lock` line for the local images.

Rebuild the rootfs yourself and confirm it matches:

```sh
make images && make images-verify
```

This is byte-for-byte reproducible on one toolchain (the same `e2fsprogs`,
`podman`, `go`, and `tar`). On a different toolchain the package set and the
SBOM still match, but the ext4 byte layout can differ.

## Uninstall

Remove `moat*` from the binary directory, the data directory, `moatd.json`, and
the systemd unit. A `moat uninstall` command is planned.
