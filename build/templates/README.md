# microVM rootfs templates

A template is a directory here with a `Dockerfile` and a `template.env`.
`build/build-template.sh` turns it into a raw ext4 rootfs that a microVM
boots. Each rootfs holds a base userland, your added packages, and the
guest agent at `/sbin/guest-agent`.

The default rootfs (`build/build-rootfs.sh`, pinned in `images.lock`) stays
the reference image. Templates are for extra userlands: a language runtime,
a build toolchain, a demo shell.

## Templates in this directory

| Name | Purpose | Size | Build |
| --- | --- | --- | --- |
| `_skeleton` | Copy this to start a new template. Not built on its own. | — | — |
| `minimal` | Alpine base plus `ca-certificates`. Fast boot for the test suite. | ~10 MiB userland, 64 MiB ext4 | `make minimal-image` |
| `dev-shell` | Python, Node, Go, C toolchain, CLI tools. Demo and manual testing. | ~400 MiB userland, 1 GiB ext4 | `make demo-image` |
| `rhel9` | RHEL 9 UBI base plus git, python3, and common CLI tools. glibc. | ~330 MiB userland, 1 GiB ext4 | `make template-image NAME=rhel9` |

## Commands

```sh
make template NAME=foo         # copy _skeleton to build/templates/foo
make template-image NAME=foo   # build build/artifacts/foo-rootfs.ext4
make demo-image                # build the dev-shell image
make minimal-image             # build the minimal image
```

`make template-image` depends on `make agent`, so the guest agent is fresh.

## Import an existing image

`moat template import` turns any linux/amd64 container image into a
template. No Dockerfile.

```sh
moat template import python:3.12            # pull and build
moat template import --from-archive img.tar # a docker-save or OCI archive
moat template import ghcr.io/org/app:v2 --name app --config moatd.json
```

It records the image `@sha256` digest and carries the image's `ENV`,
`WORKDIR`, and `ENTRYPOINT` forward: `moatd` applies them as exec
defaults, and a request value always wins. Without `--config` it writes
`<data>/<name>.template.json` and prints the snippet to paste into
`moatd.json`. See `moat template show <name>`.

Imported templates are linux/amd64 only and are not pinned in
`images.lock`.

## How the build works

1. `podman build` runs your `Dockerfile`.
2. `podman export` writes the container userland to a tar. `tar -x` unpacks
   it. There is no loop mount, so the build needs no root.
3. The build adds `/sbin/guest-agent` and the mount-point directories.
4. `find -exec touch` sets every file time to `SOURCE_DATE_EPOCH`.
5. `mkfs.ext4 -d` fills a fixed-UUID, fixed-hash-seed ext4 from the tree.

## How moatd picks a template

`moatd` reads a template list from `moatd.json` (`docs/moatd.example.json`
shows the shape). Each entry names a `rootfs` path. A client selects one by
name on `POST /v1/sandboxes` with `{"template": "dev-shell"}`. A per-token
policy can limit which templates a token may use.

## Reproducibility

The build fixes the filesystem UUID, the directory hash seed, and every
file time. Two builds on one day produce the same ext4 file.

Builds weeks apart can differ. `apk add` and `dnf install` resolve against
live distribution repos, and package versions move. For that reason
templates are **not** pinned in `images.lock`. The `packages.manifest` file
that the build writes next to each template records the exact package
versions from the last build (apk and rpm are supported). Commit it so a
reviewer sees the change.
