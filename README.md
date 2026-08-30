# microvm_controller

A Go control plane that boots, isolates, and reaps **Firecracker microVMs** as
disposable, hardware-isolated sandboxes for untrusted AI-agent code and tool use.

See [`CLAUDE.md`](CLAUDE.md) for the architecture and roadmap, and
`docs/implementation-plan.html` for the full plan.

## Status: Phase 1 — minimal viable microVM ✅

One command boots a microVM and round-trips a message with an in-VM agent over
vsock:

```
$ microvmctl demo
... microVM booted   elapsed=24ms
... Run /sbin/guest-agent as init process
guest-agent: listening on vsock port 5252
... vsock echo round-trip OK   sent=ping recv=ping
... microVM stopped
```

What runs: the host CLI (`cmd/microvmctl`) uses `firecracker-go-sdk` to start a
Firecracker process, boots a downloaded CI `vmlinux` with a minimal ext4 rootfs,
and our static guest agent (`guest/agent`) runs as PID 1 and serves a vsock echo
on port 5252. The host connects through Firecracker's vsock mux socket
(`internal/vsockconn`).

## Requirements

- Linux with `/dev/kvm` readable by your user (bare metal, or a VM with nested
  virtualization)
- `firecracker` and `jailer` on `PATH` (tested with v1.16.1)
- Go 1.27+
- `podman` and `mkfs.ext4` (to build the rootfs — no root needed)
- `curl` (to fetch the kernel)

## Build & run

```sh
# host CLI + static guest agent
go build -o bin/microvmctl ./cmd/microvmctl
CGO_ENABLED=0 go build -ldflags='-s -w' -o bin/guest-agent ./guest/agent

# VM images -> build/artifacts/{vmlinux,rootfs.ext4}
build/fetch-kernel.sh  build/artifacts/vmlinux
build/build-rootfs.sh  build/artifacts/rootfs.ext4  bin/guest-agent

# boot + vsock round-trip
bin/microvmctl demo
```

With `make` installed, `make demo` does all of the above.

### Useful flags

```
microvmctl demo -msg "hello"     # echo a custom string through the guest
microvmctl demo -keep            # leave the VM running (Ctrl-C to detach)
microvmctl demo -mem 512 -vcpus 2
microvmctl demo -jail            # run under the jailer (needs root)
```

## Layout

| Path | Purpose |
| --- | --- |
| `cmd/microvmctl` | host control CLI (Phase 1: `demo`) |
| `internal/config` | machine config, paths, kernel args |
| `internal/vm` | Firecracker driver (firecracker-go-sdk wrapper) |
| `internal/vsockconn` | host→guest vsock dial through Firecracker's mux socket |
| `guest/agent` | in-VM agent; runs as PID 1, vsock echo (grows into exec/file I/O in Phase 2) |
| `build/` | `fetch-kernel.sh`, `build-rootfs.sh` |
| `run/` | per-VM runtime state (gitignored) |
| `build/artifacts/` | downloaded/built VM images (gitignored) |

## Not yet (see roadmap in `CLAUDE.md`)

Networking, cgroups, snapshot/restore + warm pool, the northbound API — Phases 2–5.
