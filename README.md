# Moat

A Go control plane that starts, isolates, and reaps **Firecracker microVMs** as
disposable, hardware-isolated sandboxes for untrusted AI-agent code and tool use.

See [`CLAUDE.md`](CLAUDE.md) for the architecture and roadmap, and
`docs/implementation-plan.html` for the full plan.

## Status

- **Phase 1 ✅** — one command starts a microVM and reaches an in-VM agent over vsock.
- **Phase 2 ✅** — the agent runs processes and moves files, over a gRPC service
  (`agent.v1.Agent`) on vsock.
- **Phase 3 (networking) ✅** — each microVM gets its own network namespace, TAP, and
  `nftables` rules: internet egress, but no path to the host, the other microVMs, or
  `169.254.169.254`.
- **Phase 3 (limits) ✅** — cgroup v2 `cpu.max` / `memory.max` / `pids.max` on the
  Firecracker process (via this process's delegated systemd subtree, no root
  needed), plus Firecracker per-VM token-bucket rate limiters on the rootfs and the
  network interface.
- **Phase 4 ✅** — full snapshot/restore (`~25 ms` to a ready agent vs `~1.1 s` cold),
  a warm pool of microVMs (`p50` acquire `~1 ms`, with lease TTL / idle reaping), an
  orphan reaper for Firecracker processes a crash left behind, a userfaultfd
  page-fault handler (`restore -uffd`) that streams guest pages on demand, and diff
  snapshots (`snapshot -diff-exec`, `restore -diff`) whose memory file holds only
  the changed pages (`~2 MiB` on disk for a `256 MiB` guest).
- **Phase 5 ✅** — `moatd`, the northbound HTTP/JSON API: create/exec/files/delete
  sandboxes with bearer-token auth, concurrency quotas, templates, TTLs, a warm pool,
  Prometheus metrics, structured logs, and an egress domain-filter proxy (also a
  transparent mode, `-egress-transparent`, that an nftables redirect forces
  guest 80/443 through — SNI/Host filtered, no TLS termination). A `-config FILE`
  (JSON) defines templates, tokens, and a per-token policy (template allowlist,
  max concurrent, max TTL, deny networking). See [`docs/api.md`](docs/api.md) and
  [`docs/moatd.example.json`](docs/moatd.example.json).
- **Ingress secret firewall ✅** — every file, env value, and stdin crossing
  into a sandbox is scanned with the vendored Gitleaks rule set. A hit returns
  `409` (or is redacted with `-secret-redact`); a permitted token may override.
  `moat run` scans `-put` / `-env` client-side. `internal/secretscan`.
- **Agent integration ✅** — `moat-mcp` is an MCP server (stdio) giving an agent
  `moat_create` / `moat_write_file` / `moat_exec` / `moat_read_file` /
  `moat_list` / `moat_destroy`. Point it at a running `moatd` with `MOAT_ADDR` +
  `MOAT_TOKEN`. `SKILL.md` is the agent playbook. `moat doctor` preflights the
  host. HTTP client: `internal/moatapi`.

### Phase 1: smoke test

```
$ moat demo
... microVM started   elapsed=24ms
guest-agent: serving agent.v1.Agent on vsock port 5252
... agent Info OK   agent_version=phase2 guest_pid=1 kernel=6.1.182
... microVM stopped
```

### Phase 2: run a command in a fresh microVM

```
$ moat run -- uname -a
Linux (none) 6.1.182 #1 SMP ... x86_64 Linux
... command finished   code=0 pid=266

$ moat run -- sh -c 'echo out; echo err 1>&2; exit 7'; echo "exit=$?"
out
err
exit=7

# copy a file in, run against it, copy a result out
$ moat run -put ./data.csv:/tmp/data.csv -get /tmp/out.json:./out.json \
    -- sh -c 'wc -l /tmp/data.csv > /tmp/out.json'

$ moat run -tty -- top -b -n1        # PTY-attached
$ moat run -timeout 5s -- sleep 60   # exit 124 on timeout

# isolated networking (needs root, or run the whole command under
#   unshare --user --map-root-user --mount --net --fork -- ... )
$ moat run -net -- sh -c 'wget -qO- https://example.com | head -1'
$ moat run -net -- ping -c1 169.254.169.254   # blocked by the isolation rules
```

### Phase 4: snapshot, restore, warm pool

```
$ moat snapshot -id tpl -exec 'apk add --no-cache python3'   # boot, prep, snapshot
$ moat restore  -id tpl -- python3 --version                 # ~25 ms to a ready agent
... restored, agent ready   elapsed=24ms

$ moat restore -id tpl -uffd -- free -m    # pages stream from mem.bin on demand
                                                 # (needs root / vm.unprivileged_userfaultfd=1)

# diff snapshot: full base, then a memory diff of the changes made after it
$ moat snapshot -id tpl -exec 'apk add python3' -diff-exec 'pip install requests'
... full snapshot written   mem_bytes=268435456
... diff snapshot written   mem_apparent=268435456   mem_disk_bytes=1949696
$ moat restore -id tpl -diff -- python3 -c 'import requests'   # rebases base+diff

$ moat bench -pool 4 -iters 25
pool warm   elapsed=1.2s   size=4
acquire over 25 cycles:  min 0.5ms  p50 1ms  p90 2ms  p99 2ms  max 2ms
PASS: p50 acquire < 50ms

$ moat reap        # kill Firecracker processes a crash left behind
reaped 1: [orphan1]
```

### Phase 5: the daemon

```
$ MOAT_TOKENS="dev:s3cr3t" moatd -addr :8080 -pool 4
$ T='Authorization: Bearer s3cr3t'
$ ID=$(curl -s -XPOST -H "$T" localhost:8080/v1/sandboxes | jq -r .id)
$ curl -s -XPOST -H "$T" -d '{"argv":["uname","-a"]}' localhost:8080/v1/sandboxes/$ID/exec
{"stream":"stdout","data":"TGludXggLi4uCg=="}
{"stream":"exit","code":0,"signal":0,"timed_out":false}
$ curl -s localhost:8080/metrics | grep moat_sandboxes_active
```


The host CLI (`cmd/moat`) uses `firecracker-go-sdk` to start Firecracker with a
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
make build agent      # bin/moat + static bin/guest-agent
make images            # build/artifacts/{vmlinux,rootfs.ext4}
make demo             # Phase 1 smoke test
make run ARGS='-- uname -a'
make test             # fast unit tests
make test-integration # boots real microVMs, drives the agent (needs /dev/kvm)
```

## `moat run` flags

```
-id / -kernel / -rootfs / -vcpus / -mem / -jail / -keep   (as for demo)
-dir DIR            working directory in the guest
-env KEY=VALUE      set an environment variable (repeatable)
-tty               run the command attached to a PTY
-timeout DUR       kill the command after DUR (e.g. 30s); 0 means no limit
-put LOCAL:REMOTE  copy a file into the guest before the command (repeatable)
-get REMOTE:LOCAL  copy a file out of the guest after the command (repeatable)
-net               give the guest an isolated network (needs privilege)
-uplink NAME       parent interface to NAT onto: auto (default), none, or a name
-net-pool CIDR     address pool for guest networks (default 10.62.0.0/16)
-cpu N             cgroup CPU cap in cores (e.g. 0.5); 0 means no limit
-mem-max MiB       cgroup memory.max for the VMM process; 0 means no limit
-pids N            cgroup pids.max for the VMM process; 0 means no limit
-disk-bw BYTES     rootfs bandwidth limit in bytes/sec; 0 means no limit
-net-bw BYTES      network bandwidth limit in bytes/sec; 0 means no limit
```

Exit code: the guest process exit code; `124` on timeout; `128+signal` on a signal.

## Layout

| Path | Purpose |
| --- | --- |
| `cmd/moat` | host control CLI: `demo`, `run` |
| `proto/agent/v1` | `agent.v1.Agent` protobuf service |
| `gen/agent/v1` | generated protobuf + gRPC code (committed) |
| `internal/config` | machine config, paths, kernel args |
| `internal/vm` | Firecracker driver (firecracker-go-sdk wrapper) |
| `internal/vsockconn` | host→guest vsock dial through Firecracker's mux socket |
| `internal/agentclient` | host-side gRPC client for the in-VM agent |
| `internal/netconf` | per-VM netns + veth + TAP + `/30` IPAM + nftables isolation rules |
| `internal/cgroup` | cgroup v2 CPU / memory / PID caps on the VMM (delegated subtree, no root) |
| `internal/pool` | warm pool of running microVMs; one-shot lease + TTL/idle reaping + refill |
| `cmd/moat-uffd` | userfaultfd page-fault handler for `restore -uffd` |
| `cmd/moatd` | northbound HTTP/JSON API daemon |
| `cmd/moat-mcp` | MCP server for AI agents (stdio) |
| `internal/server` | API handlers, sandbox registry, auth, quotas, TTL janitor, metrics, secret firewall |
| `internal/moatapi` | Go client for the moatd HTTP API |
| `internal/secretscan` | ingress credential scanner (vendored Gitleaks rules) |
| `internal/egress` | CONNECT/HTTP forward proxy with a domain allow/deny policy |
| `guest/agent` | in-VM agent; PID 1; serves `agent.v1.Agent` (exec, file I/O, info) |
| `build/` | `fetch-kernel.sh`, `build-rootfs.sh` |
| `run/`, `build/artifacts/` | per-VM runtime state and VM images (gitignored) |

## Not yet (see roadmap in `CLAUDE.md`)

a snapshot-templated pool (P4), a gRPC surface (P5).
