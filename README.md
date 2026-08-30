# microvm_controller

A Go control plane that starts, isolates, and reaps **Firecracker microVMs** as
disposable, hardware-isolated sandboxes for untrusted AI-agent code and tool use.

See [`CLAUDE.md`](CLAUDE.md) for the architecture and roadmap, and
`docs/implementation-plan.html` for the full plan.

## Status

- **Phase 1 ✅** — one command starts a microVM and reaches an in-VM agent over vsock.
- **Phase 2 ✅** — the agent runs processes and moves files, over a gRPC service
  (`agent.v1.Agent`) on vsock.

### Phase 1: smoke test

```
$ microvmctl demo
... microVM started   elapsed=24ms
guest-agent: serving agent.v1.Agent on vsock port 5252
... agent Info OK   agent_version=phase2 guest_pid=1 kernel=6.1.182
... microVM stopped
```

### Phase 2: run a command in a fresh microVM

```
$ microvmctl run -- uname -a
Linux (none) 6.1.182 #1 SMP ... x86_64 Linux
... command finished   code=0 pid=266

$ microvmctl run -- sh -c 'echo out; echo err 1>&2; exit 7'; echo "exit=$?"
out
err
exit=7

# copy a file in, run against it, copy a result out
$ microvmctl run -put ./data.csv:/tmp/data.csv -get /tmp/out.json:./out.json \
    -- sh -c 'wc -l /tmp/data.csv > /tmp/out.json'

$ microvmctl run -tty -- top -b -n1        # PTY-attached
$ microvmctl run -timeout 5s -- sleep 60   # exit 124 on timeout
```

The host CLI (`cmd/microvmctl`) uses `firecracker-go-sdk` to start Firecracker with a
downloaded CI `vmlinux` and a minimal ext4 rootfs. The static guest agent
(`guest/agent`) runs as PID 1 and serves `agent.v1.Agent` on vsock port 5252. The host
client (`internal/agentclient`) dials it as gRPC through Firecracker's vsock mux socket
(`internal/vsockconn`).

## Requirements

- Linux with `/dev/kvm` readable by your user (bare metal, or a VM with nested
  virtualization)
- `firecracker` and `jailer` on `PATH` (tested with v1.16.1)
- Go 1.27+, `make`
- `podman` and `mkfs.ext4` (to build the rootfs — no root needed)
- `curl` (to fetch the kernel)
- To regenerate protobuf: `make tools` (installs `buf` + plugins into `$GOBIN`, no root)

## Build & run

```sh
make build agent      # bin/microvmctl + static bin/guest-agent
make images            # build/artifacts/{vmlinux,rootfs.ext4}
make demo             # Phase 1 smoke test
make run ARGS='-- uname -a'
make test             # fast unit tests
make test-integration # boots real microVMs, drives the agent (needs /dev/kvm)
```

## `microvmctl run` flags

```
-id / -kernel / -rootfs / -vcpus / -mem / -jail / -keep   (as for demo)
-dir DIR            working directory in the guest
-env KEY=VALUE      set an environment variable (repeatable)
-tty               run the command attached to a PTY
-timeout DUR       kill the command after DUR (e.g. 30s); 0 means no limit
-put LOCAL:REMOTE  copy a file into the guest before the command (repeatable)
-get REMOTE:LOCAL  copy a file out of the guest after the command (repeatable)
```

Exit code: the guest process exit code; `124` on timeout; `128+signal` on a signal.

## Layout

| Path | Purpose |
| --- | --- |
| `cmd/microvmctl` | host control CLI: `demo`, `run` |
| `proto/agent/v1` | `agent.v1.Agent` protobuf service |
| `gen/agent/v1` | generated protobuf + gRPC code (committed) |
| `internal/config` | machine config, paths, kernel args |
| `internal/vm` | Firecracker driver (firecracker-go-sdk wrapper) |
| `internal/vsockconn` | host→guest vsock dial through Firecracker's mux socket |
| `internal/agentclient` | host-side gRPC client for the in-VM agent |
| `guest/agent` | in-VM agent; PID 1; serves `agent.v1.Agent` (exec, file I/O, info) |
| `build/` | `fetch-kernel.sh`, `build-rootfs.sh` |
| `run/`, `build/artifacts/` | per-VM runtime state and VM images (gitignored) |

## Not yet (see roadmap in `CLAUDE.md`)

Networking + cgroups (P3), snapshot/restore + warm pool + orphan reaper (P4), the
public northbound API + metrics (P5).
