# Moat — Developer Guide

Moat is a Go control plane that starts, isolates, and reaps **Firecracker
microVMs** as disposable, hardware-isolated sandboxes for untrusted AI-agent
code and tool use. One microVM runs one task session. Moat never reuses it.

This guide is for a developer who wants to read the code, change it, or deploy
`moatd` in their own environment. It works from the bottom up: the isolation
boundary first, then each package, then the image pipeline, then the build,
the API, and deployment.

- API reference: [`docs/api.md`](api.md)
- Install steps: [`docs/install.md`](install.md)
- Design rationale (ADR log `DD-01`..`DD-12`): the ClaudeVault "Design
  Decisions" note
- Footguns: the ClaudeVault "Gotchas" note (`G-1`..`G-19`)

---

## 1. Architecture at a glance

```
                       ┌─────────────────────────────────────────────┐
   client / agent ───▶ │  moatd  (cmd/moatd, internal/server)         │
   HTTP + bearer token  │  auth · quotas · templates · TTL janitor    │
                        │  Prometheus /metrics · JSON request logs     │
                        └───┬───────────────┬───────────────┬─────────┘
                            │               │               │
             internal/pool  │   internal/vm │  internal/egress (host proxy)
             warm pool of    │  Firecracker  │  domain allow/deny · forward
             running VMs      │  + jailer     │  + transparent (DNAT)
                            │               │
                     ┌──────▼───────────────▼─────────┐
                     │  one microVM per task session   │
                     │  ┌───────────────────────────┐  │
                     │  │ guest/agent as PID 1       │  │
                     │  │ gRPC over AF_VSOCK :5252   │  │
                     │  │ Exec · WriteFile · ReadFile│  │
                     │  └───────────────────────────┘  │
                     │  netns + TAP + /30 · cgroup v2   │
                     │  shared ro rootfs.ext4 + kernel  │
                     └─────────────────────────────────┘
```

**Request flow for `POST /v1/sandboxes` then `POST .../exec`:**

1. `moatd` authenticates the bearer token, checks the per-token and global
   concurrency quota, and resolves the template.
2. It takes a warm VM from `internal/pool`, or cold-boots one through
   `internal/vm.Boot`. A networked sandbox also gets a network namespace, a
   TAP device, a `/30` address, and `nftables` isolation rules from
   `internal/netconf`.
3. It dials the guest agent over vsock (`internal/agentclient`) and calls
   `Info` as a readiness check.
4. `exec` opens a gRPC `Exec` stream to the agent. The agent spawns the
   process, streams stdout and stderr back as NDJSON frames, and sends one
   exit frame.
5. `DELETE` (or the TTL janitor, or shutdown) stops the Firecracker process,
   removes the cgroup, and tears down the network namespace.

The audit log (`internal/audit`) records every boundary crossing as a
hash-chained JSONL event.

---

## 2. Repository layout

| Path | Lines | Responsibility |
| --- | ---: | --- |
| `cmd/moat` | ~1540 | host CLI: `demo`, `run`, `snapshot`, `restore`, `bench`, `reap`, `doctor`, `template`, `audit` |
| `cmd/moatd` | ~690 | the northbound HTTP daemon and its config loader |
| `cmd/moat-mcp` | ~440 | Model Context Protocol server (stdio) for AI agents |
| `cmd/moat-uffd` | ~290 | userfaultfd page-fault handler for `restore -uffd` |
| `guest/agent` | ~470 | the in-VM agent: PID 1, mounts pseudo-fs, reaps children, serves `agent.v1.Agent` over vsock |
| `internal/vm` | ~1225 | the Firecracker driver: `Boot`, `Restore`, snapshot and diff snapshot, `Reconcile` |
| `internal/agentclient` | ~410 | host-side gRPC client for the guest agent |
| `internal/vsockconn` | ~190 | host→guest dial through the Firecracker vsock multiplex socket |
| `internal/config` | ~380 | `Machine` config, kernel args, run and data directory resolution |
| `internal/netconf` | ~920 | per-VM netns + veth + TAP + `/30` IPAM + `nftables` rules |
| `internal/cgroup` | ~230 | cgroup v2 `cpu.max` / `memory.max` / `pids.max` on the VMM process |
| `internal/pool` | ~450 | warm pool of running microVMs, leases with TTL and idle timeout |
| `internal/server` | ~3000 | the HTTP API, auth, quotas, templates, janitor, metrics, and the image check |
| `internal/egress` | ~800 | forward and transparent egress proxy with a domain policy |
| `internal/secretscan` | ~470 | ingress secret firewall (vendored Gitleaks rules, in-repo evaluator) |
| `internal/audit` | ~2100 | append-only, hash-chained audit log: writer, reader, per-sandbox report |
| `internal/imagelock` | ~290 | load and verify `images.lock` at daemon startup |
| `internal/moatapi` | ~490 | Go client for the moatd HTTP API |
| `build/` | — | `fetch-kernel.sh`, `build-rootfs.sh`, `sbomgen`, `gen-images-lock.sh`, `images-verify.sh`, `release.sh` |
| `proto/agent/v1` | — | the guest-agent protobuf; `gen/` holds the committed generated code |
| `deploy/` | — | `moatd.service` systemd unit |

`internal/` is a Go internal package tree. Nothing outside the module imports
it. External integrations use `internal/moatapi` or the HTTP API directly.

---

## 3. The security model

Moat runs code it does not trust. The isolation is layered so that a break in
one layer still meets another.

### 3.1 The layers, from the guest outward

| Layer | Mechanism | What it stops |
| --- | --- | --- |
| **CPU / memory isolation** | Firecracker microVM on KVM. A separate kernel, a minimal device model (virtio-blk, virtio-net, virtio-vsock, a serial port, a one-button controller). | A guest kernel exploit reaches a Firecracker VMM, not the host kernel directly. The attack surface is far smaller than a full QEMU. |
| **VMM confinement** | The **jailer** (`-jail`): chroot, a seccomp filter on the VMM syscalls, dropped capabilities, a dedicated uid/gid, its own cgroup. Or, without root, a plain unprivileged process. | A VMM compromise runs as an unprivileged, chrooted, seccomp-filtered process. |
| **Resource caps** | cgroup v2 `cpu.max`, `memory.max`, `pids.max` on the VMM process (`internal/cgroup`). Firecracker per-VM token-bucket rate limiters on the rootfs block device and the network interface. | A guest cannot starve the host of CPU, memory, PIDs, disk bandwidth, or network bandwidth. |
| **Network isolation** | One network namespace per VM. A veth pair to the host side, a TAP device inside, a `/30` point-to-point subnet (`internal/netconf`). `nftables` in the namespace **drops** traffic to the host, to other microVMs, to `169.254.0.0/16` (link-local and cloud metadata), to loopback, and to every RFC-1918 range. Egress leaves only through the host proxy. | A guest cannot reach the host, another sandbox, the cloud metadata endpoint, or the host LAN. It can only reach the internet through a filtered proxy. |
| **Egress filtering** | A host forward proxy (`internal/egress`). Networked sandboxes get `HTTP(S)_PROXY` pointed at it. A domain allow / deny policy decides each request. A transparent listener plus an `nftables` DNAT on guest TCP 80 and 443 stops a guest that ignores the proxy environment variables. | A guest cannot exfiltrate to an arbitrary domain. The policy is per deployment, and per token through `moatd.json`. |
| **Storage isolation** | A shared, read-only `rootfs.ext4` plus a per-VM ephemeral scratch disk. Files move over vsock only. No virtio-fs, no NFS, no shared mount. | A guest cannot see the host filesystem or another sandbox's files. (See the note in 3.4 on the current cold-boot path.) |
| **One VM per session** | `moatd` never hands the same microVM to a second request. `DELETE`, the TTL, and shutdown destroy it. | No state, no credential, and no implanted process survives from one task to the next. |
| **Ingress secret firewall** | Every file, environment value, and stdin that crosses into a sandbox is scanned for credentials (`internal/secretscan`). A hit returns `409`, or a redacted copy, or an audited override. | A real credential does not enter an untrusted sandbox by mistake. |
| **Image integrity** | `moatd` verifies the kernel and the rootfs against a checked-in `images.lock` at startup and fails closed on a mismatch (`internal/imagelock`). | A tampered or unexpected image does not serve traffic. |
| **Audit trail** | Every boundary crossing is a hash-chained JSONL event (`internal/audit`). Events are metadata only. | An operator can reconstruct exactly what a sandbox did, and detect a break in the chain. |

### 3.2 Threat model

- **Trusted:** the host kernel, KVM, Firecracker and the jailer, `moatd`, the
  guest agent binary, the pinned kernel and base image.
- **Untrusted:** every process the guest runs, every byte the guest writes,
  every network packet the guest sends, and every file a caller asks to put
  into a sandbox.
- **Out of scope:** a KVM or a hardware (CPU microcode, speculative
  execution) vulnerability. A malicious host operator. A GPU workload
  (Firecracker has no VFIO — see `DD-11`).

### 3.3 Design decisions that shape the model

`DD-01` Firecracker as the boundary. `DD-02` a custom Go daemon on
`firecracker-go-sdk`, not `firecracker-containerd` or Kata. `DD-06`
netns + TAP + `/30` + `nftables` + an egress proxy. `DD-07` shared read-only
rootfs plus per-VM CoW scratch, files over vsock only. `DD-08` one microVM
per task session, never reused. Each decision records the alternatives it
rejected.

### 3.4 Known gaps

- **Cold-boot rootfs is read-write.** `internal/vm.Boot` attaches the root
  drive with `IsReadOnly: false`, and `coldBoot` in `internal/server` points
  the machine straight at the template `rootfs.ext4`. Only the warm pool
  copies the rootfs per member. A non-pooled sandbox therefore writes into
  the shared template image for the life of that boot. Gotcha `G-19`. The fix
  (attach the root drive read-only, or copy per sandbox) is a planned
  VM-driver change.
- **The jailer path (`-jail`) and real-uplink NAT (`-uplink <iface>`) need a
  real root host** to exercise. They are coded and tested on a root box, not
  in the rootless dev flow.
- **UFFD restore needs `/dev/userfaultfd`** or
  `vm.unprivileged_userfaultfd=1` (Gotcha `G-14`).

---

## 4. Core components, low level

### 4.1 The Firecracker driver — `internal/vm`

`Boot(ctx, m config.Machine, log) (*Instance, error)` starts a microVM from a
cold kernel and rootfs. It builds a `firecracker.Config` (`baseConfig`), one
root drive, one vsock device, and, when `m.Net` is set, one network
interface. `startMachine` then applies either the jailer
(`cfg.JailerCfg`, when `m.Jail`) or the plain unprivileged process runner,
starts the VMM, and waits for the guest agent to answer on vsock.

`Instance` holds the `*firecracker.Machine` (or a raw API client for the UFFD
path), the VMM `pid`, and the run directory. `Instance.Stop` sends the VMM a
shutdown, waits, and kills the process if it does not exit. `Instance.PID`
returns the VMM pid so `internal/cgroup` can cap it.

**Snapshot and restore.**

- `Instance.CreateSnapshot(ctx, dir)` pauses the VM, writes `snapshot.bin`
  and `mem.bin`, and leaves the VM paused. `moat snapshot`.
- `Restore(ctx, m, dir, log)` starts a fresh VMM from a full snapshot.
  Restore reaches a ready agent in about 25 ms against roughly 1.1 s for a
  cold boot.
- `RestoreForDiff` restores with dirty-page tracking on, then
  `CreateSnapshotDiff` writes a sparse `mem.bin` with only the changed pages.
  `RestoreDiff` rebases the sparse diff onto a copy of the base memory
  (`RebaseMemory`, using `SEEK_DATA` / `SEEK_HOLE`) and restores from the
  merge. A diff snapshot must come from a fresh restore, never a resumed
  instance (Gotcha `G-12`).
- `RestoreUFFD` (`internal/vm/rawfc.go` plus `cmd/moat-uffd`) does a raw
  `snapshot/load` with `mem_backend`, and a separate handler process streams
  guest pages from `mem.bin` on demand through a `userfaultfd`.

**Reconcile.** `Reconcile(runRoot, keep, log)` reads every `firecracker.pid`
under the run root, checks the pid still names a Firecracker process, and
kills the ones a crash left behind. `moatd` calls it at startup. `moat reap`
calls it by hand.

**Gotcha:** `PauseVM` / `ResumeVM` on the same VMM process breaks the guest
vsock on this Firecracker build (`G-12`). The warm pool therefore keeps its
members **running**, not paused. Snapshot restore, which starts a fresh
process, is unaffected.

### 4.2 The guest agent — `guest/agent`

The agent is a static (`CGO_ENABLED=0`) binary. The kernel runs it as PID 1
through `init=/sbin/guest-agent`. As PID 1 it must never return — the kernel
panics if init exits — so every fatal path calls `haltForever`.

On start it:

1. mounts `/proc`, `/sys`, `/dev` (devtmpfs, tolerating `EBUSY` because the
   Firecracker CI kernel auto-mounts it), `/dev/pts`, `/dev/shm`, and a tmpfs
   `/run`;
2. sets a base `PATH` and writes a default `/etc/resolv.conf`;
3. installs a `SIGCHLD` reaper so orphaned processes do not become zombies;
4. listens on `AF_VSOCK` port 5252 and serves the `agent.v1.Agent` gRPC
   service.

**The RPC surface** (`proto/agent/v1/agent.proto`, generated into `gen/`):

- `Info(InfoRequest) → InfoResponse` — agent version, pid, `uname -r`, uptime.
  The host calls it as a readiness check.
- `Exec(ExecRequest) → stream ExecEvent` — one process. The request carries
  `argv`, `env`, `dir`, `stdin`, and a `timeout_ms`, plus `tty` with
  `tty_cols` / `tty_rows` for a PTY. The stream carries a `Started` event
  (with the pid), then `stdout` and `stderr` byte chunks, then one `Exit`
  event with `code`, `signal`, and `timed_out`. On timeout the agent kills
  the whole process group.
- `WriteFile(stream WriteFileChunk) → WriteFileResponse` — the first chunk
  sets `path` and `mode`, every chunk carries `data`. The response returns
  `bytes_written` and the hex SHA-256.
- `ReadFile(ReadFileRequest) → stream FileChunk` — data chunks, then a final
  chunk with `eof`, `size`, and `sha256` and no data.

Files move only through `WriteFile` / `ReadFile` over vsock. There is no
shared mount.

### 4.3 The vsock transport — `internal/vsockconn`, `internal/agentclient`

Firecracker multiplexes guest vsock connections over one host-side Unix
socket (`m.VsockUDSPath()`). To open a guest connection the host connects to
that socket and writes `CONNECT <port>\n`. The mux replies `OK <hostport>\n`
and then the stream is a raw pipe to the guest listener.

`vsockconn` implements that handshake and a `WaitReady` retry loop.
`agentclient` wraps it in a gRPC `ClientConn` with a custom dialer, so the
host code calls `client.Exec(...)` and `client.WriteFile(...)` like any gRPC
client. Host CID is always 2. The first guest is CID 3
(`config.GuestCID`).

### 4.4 Networking — `internal/netconf`

`Apply(Plan)` and `Destroy(Plan)` need `CAP_NET_ADMIN` and `CAP_SYS_ADMIN` —
real root, or `unshare --user --map-root-user --mount --net`.

A `Plan` (from `BuildPlan(lease, uplink)`) describes:

- a named network namespace (`/run/netns/mvm-<id>`, or a rootless location
  when `/run/netns` is not writable — Gotcha `G-10`);
- a veth pair — one end in the host netns, one end (`VethNS`) in the VM
  netns — on a `/30`;
- a TAP device inside the VM netns on a second `/30`, which Firecracker
  attaches as the guest NIC;
- a static guest address from the `/16` pool (`DefaultPool = 10.62.0.0/16`),
  handed out `/30` at a time by `internal/netconf.Allocator`;
- the kernel `ip=` argument so the guest configures the NIC at boot with no
  DHCP.

`nftScript()` writes this `table inet mvm` into the VM netns:

```
chain forward {
  type filter hook forward priority 0; policy accept;
  ct state established,related accept
  ip daddr { <tap /30>, <veth /30> } accept        # the point-to-point links
  ip daddr 169.254.0.0/16 drop                      # link-local + cloud metadata
  ip daddr 127.0.0.0/8   drop
  ip daddr 10.0.0.0/8    drop                       # RFC-1918
  ip daddr 172.16.0.0/12 drop
  ip daddr 192.168.0.0/16 drop
}
chain postrouting {
  type nat hook postrouting priority 100; policy accept;
  oifname "<veth host side>" masquerade
}
```

With `RedirectPort > 0` it also adds a `prerouting` DNAT that forces guest
TCP 80 and 443 to the transparent egress proxy on the host side, so the guest
cannot bypass the proxy.

The guest keeps a route to the internet through the veth, then the host
masquerades it. Everything private is dropped first.

### 4.5 Resource limits — `internal/cgroup` and Firecracker rate limiters

`cgroup.Apply(name, pid, Limits) (remove func(), error)` writes `cpu.max`,
`memory.max`, and `pids.max` into a new leaf under this process's **delegated
systemd subtree** (`…/user@<uid>.service/microvm/mvm-<id>`). Because the write
stays inside the delegated subtree, it needs no root when systemd delegation
is in effect. `cgroup.Available()` preflights. `Instance.Stop` calls the
returned `remove` after the VMM exits.

`Limits` is `{CPUCores float64, MemBytes int64, PIDs int64}`. A zero field
means no cap for that resource.

Firecracker's own per-VM token-bucket rate limiters cap the rootfs block
device and the network interface bandwidth
(`vm.bandwidthLimiter(bytesPerSec)` builds a `models.RateLimiter` with a
one-second refill).

### 4.6 The warm pool — `internal/pool`

`pool.New(ctx, Options{Size}, base config.Machine, log)` starts `Size`
microVMs from a copy of the base rootfs and keeps them **running** and idle.

`Acquire(ctx)` returns a `*Lease` — the fastest ready member, p50 about 1 ms.
A `Lease` carries a TTL and an idle timeout. `Lease.Touch()` resets the idle
clock. A background `sweep` force-releases a lease that outruns either
deadline. `Lease.Release()` discards the member (one VM per session) and
`refill` starts a replacement.

`moatd -pool N` turns it on. The pool serves only non-networked sandboxes.
`moat bench` exercises it.

### 4.7 The northbound API — `internal/server`

`server.New(cfg Config, log) (*Server, error)` validates the config, builds
the template map, runs the image check (4.11), builds the secret scanner
unless it is off, registers metrics, and returns the server. `Start(ctx)`
starts the warm pool if configured, writes the `daemon.start` audit event,
and starts the TTL janitor. `Shutdown(ctx)` writes `daemon.stop`, destroys
every sandbox, and waits for in-flight teardowns.

**Routes** (Go 1.22 `http.ServeMux` patterns, all behind `s.auth`):

| Method + path | Handler |
| --- | --- |
| `GET /healthz` | liveness, no auth |
| `GET /metrics` | Prometheus text, no auth |
| `POST /v1/sandboxes` | create — quota check, template resolve, boot |
| `GET /v1/sandboxes` | list the caller's sandboxes |
| `GET /v1/sandboxes/{id}` | one sandbox |
| `DELETE /v1/sandboxes/{id}` | destroy |
| `POST /v1/sandboxes/{id}/exec` | stream a command as NDJSON |
| `PUT /v1/sandboxes/{id}/files/{path...}` | write a file (scanned) |
| `GET /v1/sandboxes/{id}/files/{path...}` | read a file, `X-Sha256` trailer |
| `GET /v1/sandboxes/{id}/report` | per-sandbox audit rollup |
| `GET /v1/audit` | the audit stream (NDJSON), per-token scope |

**Auth** is a bearer token. `Config.Tokens` maps token id → secret.
`Config.Policies` maps token id → `TokenPolicy` (a template allowlist, a
per-token concurrency cap, a TTL cap, `deny_network`, `allow_secret_override`,
`audit_read: "all"`).

**Quotas.** `registry.reserve` / `release` enforce `Config.GlobalMax` and the
per-token cap (`TokenPolicy.MaxConcurrent`, else `Config.TokenMax`). A
blocked request gets `429` and a `quota.reject` audit event.

**The janitor** destroys a sandbox when its TTL passes and writes a
`sandbox.destroy` event with `reason: "ttl"`.

### 4.8 The egress proxy — `internal/egress`

`Proxy` serves two listeners:

- **Forward** (`Serve`): handles `CONNECT` (HTTPS tunnels, filtered by the
  requested host name, no TLS interception) and absolute-URI HTTP. A
  networked sandbox gets `HTTP_PROXY` / `HTTPS_PROXY` pointed here.
- **Transparent** (`ServeTransparent`): takes raw connections that the
  namespace DNAT rule redirected. It recovers the pre-DNAT address with
  `SO_ORIGINAL_DST`, reads the host from the HTTP request line or the TLS
  SNI (no termination), applies the same policy, and splices.

`Policy{Allow, Deny []string}`. `Policy.Match(host)` returns the deciding
pattern and the allow decision. A host is allowed when it matches no `Deny`
entry and (`Allow` is empty or it matches an `Allow` entry). Matching is
case-insensitive on the exact name or any parent domain.

`decide` records an `egress.allow` or `egress.deny` audit event with the
host, the port, the method, the matched rule, the `via` listener, and the
resolved sandbox and token (from the client IP through
`Server.IdentifyClient`). Both event kinds go on the async audit path so a
guest cannot flood the audit writer with denied connections (Gotcha `G-18`
is about the shell, not this — this is a design point from the M2 review).

### 4.9 The ingress secret firewall — `internal/secretscan`

`secretscan.New()` loads the vendored Gitleaks rule set
(`internal/secretscan/gitleaks.toml`, MIT). The Gitleaks module itself is
**not** imported — it pulls 200+ dependencies. A slim in-repo evaluator runs
each rule: a keyword prefilter, then the regex, then an optional Shannon
entropy floor, then the allowlists.

`Scan(name, content) []Finding` returns the rule id, the line, the entropy,
and the byte span of each hit. `Redact(content, findings)` replaces every
span with a marker.

`moatd` calls it on every `-env` value, every stdin, and every file body on
the way in. A hit returns `409`, unless the token has
`allow_secret_override` and the request sets `allow_secrets: true` (audited),
or `moatd -secret-redact` is on (the marker replaces the secret and the
request proceeds). `moat run` scans `-put` files and `-env` values
client-side, with `-allow-secret` to override.

`secret.block` / `secret.allow` / `secret.redact` audit events record only
the target, the deduped rule ids, and a count. The matched value never
leaves the scanner. A failed `secret.allow` or `secret.redact` audit write
returns `503` — the content does not enter the sandbox with no durable
record.

### 4.10 The audit log — `internal/audit`

An append-only JSONL log with a SHA-256 hash chain. Each event has a `prev`
(the previous event's hash) and a `hash` (the SHA-256 of this event's
canonical form: keys sorted at every level, the `hash` field omitted,
`ts` in RFC3339Nano UTC). The first event's `prev` is 64 zeros.

**Write paths.**

- `RecordSync(ctx, Event) error` — for boundary events (create, destroy,
  daemon start and stop, quota reject, secret allow, orphan reap). It
  appends, `fsync`s, and returns. On an `fsync` or a partial-write failure it
  rolls back `seq`, truncates the partial bytes, and returns the error, so a
  failed event never forks the chain.
- `Record(Event)` — for hot events (exec, file, egress). It never blocks. It
  enqueues on a buffered channel (cap 8192). A background `writeLoop` batches
  `fsync` calls. A full queue drops the event and increments
  `moat_audit_dropped_total`.
- `Flush()` sends a sentinel through the queue and waits, so a following
  sync event sorts after the async events already queued.

**Rotation and retention.** Daily files
`audit-YYYY-MM-DD.NNNNN.jsonl`, zero-padded so `sort.Strings` equals write
order. Retention removes files by age (mtime), then by total size, and never
the current file.

**Reading.** `Query(dir, Filter)` scans the files (bounded to `limit` matched
events plus one file in memory). `Verify(dir)` recomputes every hash and
checks the `prev` links and the seq continuity. A truncated final line is
tolerated. A corrupt complete line before the end is an error — and `moatd`
refuses to start (fail-closed). Recovery is `moat audit verify -dir <dir>`,
then move the bad file aside.

**Read scope.** A token with `audit_read: "all"` sees every event and can run
verify over the API. Any other token sees only events for sandboxes it
created plus events tagged with its own token id.

### 4.11 Image verification — `internal/imagelock`

`images.lock` records the SHA-256 of the kernel, the rootfs, and the SBOM,
plus provenance (the base image digest, the kernel CI snapshot, the ext4
UUID, the `mke2fs` version, the Go version). See section 5.

`imagelock.Find(rootfsPath)` looks for `images.lock` beside the rootfs, then
in the parent directory. `Load` parses it and rejects an unknown schema.
`(*Lock).VerifyFiles(kernel, rootfs)` streams SHA-256 over both files and
returns a typed `*MismatchError` on a difference.
`(*Lock).VerifySignature(...)` runs `cosign verify-blob` when an identity is
configured.

`server.New` runs `verifyImages` once, before the warm pool starts:

- `image_verify=enforce` (default): a mismatch drops that template. If no
  template is left, `New` returns an error and `moatd` exits non-zero.
- `image_verify=warn`: log at error level, keep serving.
- `image_verify=off`: skip the check.
- A missing `images.lock` is a warning in every mode, so an existing install
  still starts.

The `daemon.start` audit event carries an `images` array — one entry per
template with `kernel_sha256`, `rootfs_sha256`, `sbom_sha256`, `verified`,
and `signed`.

---

## 5. The image pipeline

`make images` runs four steps in order: `kernel`, `rootfs`, `sbom`, `lock`.
The pins live in the `Makefile` and in the two build scripts — keep them in
sync.

### 5.1 Kernel — `build/fetch-kernel.sh`

The script downloads **one pinned** `vmlinux` from the Firecracker CI S3
bucket and checks its SHA-256. It does not resolve "newest" anything. Three
constants (overridable by env):

```
KERNEL_CI_SNAPSHOT   20260826-761f88fbb951-0
KERNEL_VERSION_EXACT 6.1.182
KERNEL_SHA256        90f6c45a719c76c08f09ba5e80b4a7f1044e33152c92b9e45a22e87c0021bac5
```

An existing file is re-verified and re-downloaded on a mismatch. Moat does
not build the kernel from source — see the spec's non-goals.

### 5.2 Rootfs — `build/build-rootfs.sh`

Byte-identical on one toolchain (the same `podman`, `e2fsprogs`, `tar`,
`go`). The controls:

| Non-determinism source | Control |
| --- | --- |
| the `alpine:3.20` tag moves | pin `BASE_IMAGE=docker.io/library/alpine@sha256:c64c687c…` |
| `podman export` file times and entry order | `find "$root" -exec touch --date=@$SOURCE_DATE_EPOCH` after extract |
| the guest-agent binary embeds paths and a build id | `CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags='-s -w -buildid='`, with `toolchain go1.27.0` in `go.mod` |
| `mkfs.ext4` random UUID, hash seed, superblock times, lazy init | `E2FSPROGS_FAKE_TIME=$SOURCE_DATE_EPOCH mkfs.ext4 -U <fixed> -E hash_seed=<fixed>,lazy_itable_init=0,lazy_journal_init=0` |
| the two callers disagreed on size | `ROOTFS_SIZE` frozen at `256M` in the script |

`SOURCE_DATE_EPOCH` is `1788134400` (2026-08-31 00:00:00 UTC), a fixed
literal, overridable by env.

Reproducibility holds **for a fixed toolchain**. On a different `e2fsprogs`
the package set and the SBOM still match, but the ext4 byte layout can shift.
`moatd` enforces the digest from the lock that shipped with the image, so
this does not weaken the runtime check.

### 5.3 SBOM — `build/sbomgen`

A small Go program run with `go run ./build/sbomgen`. Never a shipped binary.
It reads:

- the Alpine package database out of the ext4 with `debugfs -R 'cat
  /lib/apk/db/installed'` (no mount, no root — Gotcha `G-4`);
- the guest-agent module list with `go version -m bin/guest-agent`.

It emits deterministic CycloneDX 1.5 JSON (hand-marshaled structs, no new
dependency). `metadata.timestamp` comes only from `-source-date-epoch`, so
two runs with the same inputs produce byte-identical output.

### 5.4 `images.lock` — `build/gen-images-lock.sh`

The script computes the three SHA-256 sums and writes the JSON
(`schema: "moat.images.lock/v1"`). `make images` writes it to
`build/artifacts/images.lock` (git-ignored). A canonical copy lives at the
**repo root** and is checked in. `generated_at` is the only non-reproducible
field.

The release tarball carries `images.lock` and `sbom.cdx.json`.
`install.sh` puts them in `<data>/` (default `/var/lib/moat`) beside the
images. To sign the lock (optional):

```
cosign sign-blob --yes --output-signature images.lock.sig images.lock
```

and set `cosign_identity` (or `MOAT_COSIGN_IDENTITY`) on `moatd`.

### 5.5 Proving reproducibility — `make images-verify`

`make images-verify` rebuilds the rootfs and the SBOM into a scratch
directory, then asserts:

1. the rebuilt `rootfs.ext4` SHA-256 equals the built one,
2. both equal `images.lock`'s `rootfs.sha256`,
3. the rebuilt `sbom.cdx.json` SHA-256 equals the built one,
4. `build/artifacts/images.lock` equals the repo-root `images.lock` minus
   `generated_at`.

It prints `reproducible: OK` or the first failing assertion.

### 5.6 Bumping the kernel or the base image

1. Resolve the new pin values (the commands are in the plan, section
   "Task 2 Step 1" and "Task 3 Step 1").
2. Edit the constants in `build/fetch-kernel.sh`, `build/build-rootfs.sh`,
   and the `Makefile`.
3. `make images && make images-verify`.
4. `cp build/artifacts/images.lock images.lock` and commit.
5. Re-sign `images.lock.sig` at release time.

### 5.7 Extra userlands — `build/templates/`

The default rootfs is the reference image and the only one in `images.lock`.
For a language runtime, a build toolchain, or a demo shell, add a
**template**: a directory under `build/templates/<name>/` with a `Dockerfile`
and a `template.env`.

`build/build-template.sh <name>` runs `podman build`, exports the userland
the same rootless way as `build-rootfs.sh` (`podman export | tar -x`, no loop
mount), adds `/sbin/guest-agent`, normalises file times to
`SOURCE_DATE_EPOCH`, and writes `build/artifacts/<name>-rootfs.ext4` with a
fixed UUID and hash seed. It records the installed packages in
`build/templates/<name>/packages.manifest`.

```sh
make template NAME=python        # copy build/templates/_skeleton to build/templates/python
make template-image NAME=python  # build build/artifacts/python-rootfs.ext4
make demo-image                  # the dev-shell toolchain image
make minimal-image               # the small test image
```

Templates are byte-stable **within a day**. `apk add` resolves against the
live Alpine index, so builds weeks apart can differ. That is why templates
are not pinned in `images.lock`. `moatd` picks a template by the `rootfs`
path in each `moatd.json` entry; a client selects one with
`{"template": "<name>"}` on `POST /v1/sandboxes`. See
`build/templates/README.md`.

---

## 6. Building and testing

### 6.1 Toolchain

- **Go 1.27.0 exactly.** `go.mod` declares `toolchain go1.27.0`. The
  `Makefile` exports `GOTOOLCHAIN=local` so `go` never downloads a toolchain.
  A reproducible `guest-agent` needs this exact version.
- `podman`, `e2fsprogs` (`mkfs.ext4`, `debugfs`), `tar`, `curl` for the
  image pipeline.
- `firecracker` and `jailer` on `PATH` (this repo tests against v1.16.1).
- KVM: `/dev/kvm` readable and writable by the build user.
- `buf`, `protoc-gen-go`, `protoc-gen-go-grpc` only to regenerate `gen/`
  (`make tools` installs them into `$GOBIN`, no root). `buf` is a single Go
  binary — there is no system `protoc`.

### 6.2 Make targets

| Target | Effect |
| --- | --- |
| `make all` | build `moat`, `moatd`, `moat-mcp`, `moat-uffd`, `guest-agent` into `bin/` |
| `make agent` | the static guest agent only (`-trimpath -buildvcs=false`) |
| `make images` | `kernel` → `rootfs` → `sbom` → `lock` into `build/artifacts/` |
| `make images-verify` | rebuild the rootfs and SBOM, assert byte-for-byte reproducible |
| `make test` | `go test ./...` — unit tests, no VM, fast |
| `make test-integration` | `go test -tags integration` — boots real microVMs, needs `/dev/kvm` and `make images` |
| `make test-integration-fast` | the same suite against the small `minimal` rootfs, no `make images` |
| `make test-net` | network isolation test, runs rootless in a user + net namespace |
| `make template NAME=x` / `make template-image NAME=x` | scaffold and build a rootfs template |
| `make demo-image` / `make minimal-image` | build the `dev-shell` and `minimal` template images |
| `make proto` | regenerate `gen/` from `proto/` (needs `make tools` once) |
| `make dist` | assemble `dist/moat_<ver>_linux_amd64.tar.gz` + `checksums.txt` |
| `make serve` | run `moatd` locally (`MOAT_TOKENS=id:secret make serve`) |
| `make demo` / `make run ARGS='-- uname -a'` | build, fetch a kernel, start a microVM |

### 6.3 Test tiers

- **Unit** (`go test ./...`): pure Go, no root, no VM, no network. This is
  the gate for every change.
- **Integration** (`-tags integration`): `internal/agentclient`,
  `internal/vm`, `internal/pool`, `internal/server`. Boots real microVMs. It
  runs `make images` first. It `t.Skip`s when `/dev/kvm` or the images are
  absent. On a box with KVM it takes roughly 60 to 90 seconds.
  `make test-integration-fast` runs the same suite against the small
  `minimal` template rootfs (`MOAT_TEST_ROOTFS`), skipping `make images`.
- **Network** (`-tags netint`, `make test-net`): builds a real per-VM network
  and checks isolation from inside the guest. Runs unprivileged inside
  `unshare --user --map-root-user --mount --net`.

A cold boot attaches the rootfs read-write, so `coldBoot` (and the warm
pool) give every sandbox a **private sparse copy** of the template rootfs in
its run directory, removed on destroy. Sandboxes never share one ext4 file
(Gotcha `G-19`).

### 6.4 Regenerating protobuf

`gen/` is committed. Change `proto/agent/v1/agent.proto`, then
`make tools` once, then `make proto`. Both `buf lint` and `buf generate` must
pass. Commit `gen/` with the proto change.

---

## 7. Using it

### 7.1 The `moat` CLI

| Command | Purpose |
| --- | --- |
| `moat demo` | start a microVM, run the agent readiness check, stop it |
| `moat run [flags] -- CMD ...` | start a one-shot microVM, run a command, tear it down |
| `moat snapshot -exec ... [-diff-exec ...] <dir>` | boot, run a warm-up command, snapshot |
| `moat restore [-diff] [-uffd] <dir> -- CMD` | restore from a snapshot and run a command |
| `moat bench` | warm-pool acquire and release benchmark |
| `moat reap` | kill Firecracker processes a crash left behind |
| `moat doctor` | preflight KVM, firecracker, cgroup delegation, nft, userfaultfd, images, `images.lock` |
| `moat template list` | show the configured template and where to add more |
| `moat audit {tail,query,report,verify}` | read the audit log, offline (`-dir`) or over the API |

`moat run` flags: `-env KEY=VALUE` (repeatable), `-put LOCAL:REMOTE`,
`-get REMOTE:LOCAL`, `-tty`, `-timeout`, `-dir`, `-net` with
`-uplink auto|none|<iface>` and `-net-pool CIDR`, `-mem-max`, `-pids`,
`-disk-bw`, `-net-bw`, `-allow-secret`, `-jail`, `-keep`.

Example:

```
moat run -net -env GOFLAGS=-mod=mod -put ./main.go:/work/main.go \
  -get /work/out.bin:./out.bin -timeout 60s -- \
  sh -c 'cd /work && go build -o out.bin ./main.go'
```

### 7.2 The HTTP API

Start the daemon:

```
MOAT_TOKENS=ci:s3cr3t moatd -pool 4
# or
moatd -config /etc/moat/moatd.json
```

A walkthrough with `curl` (token `ci`, secret `s3cr3t`):

```sh
H='-H Authorization: Bearer s3cr3t'

# create
id=$(curl -s $H -XPOST localhost:8080/v1/sandboxes -d '{"ttl_seconds":120}' | jq -r .id)

# exec — NDJSON: a started frame, stdout/stderr frames, one exit frame
curl -s $H -XPOST localhost:8080/v1/sandboxes/$id/exec \
  -d '{"argv":["sh","-c","echo hi; uname -r"]}'

# put a file (scanned for secrets on the way in)
curl -s $H -XPUT --data-binary @main.go \
  localhost:8080/v1/sandboxes/$id/files/work/main.go

# get a file back (the X-Sha256 trailer holds the digest)
curl -s $H localhost:8080/v1/sandboxes/$id/files/work/main.go

# the per-sandbox audit rollup
curl -s $H localhost:8080/v1/sandboxes/$id/report | jq

# destroy
curl -s $H -XDELETE localhost:8080/v1/sandboxes/$id
```

Full request and response shapes are in [`docs/api.md`](api.md).

### 7.3 The MCP server and `SKILL.md`

`moat-mcp` is a Model Context Protocol server over stdio
(`github.com/modelcontextprotocol/go-sdk`). It exposes `moat_create`,
`moat_list`, `moat_destroy`, `moat_exec`, `moat_write_file`,
`moat_read_file`. A secret block comes back as an `IsError` result that tells
the agent to ask the user, not to override. Point it at a running `moatd`
with `MOAT_ADDR` and `MOAT_TOKEN` (or `-addr` / `-token`).

`SKILL.md` at the repo root is the agent playbook — when to use a sandbox,
the create → put → exec → get → destroy loop, and how a blocked-secret error
looks.

### 7.4 The Go client — `internal/moatapi`

`moatapi.New(addr, token)` returns a client with `Create`, `List`,
`Destroy`, `Exec` (NDJSON assembled), `PutFile` (returns a
`*SecretBlockedError` on `409`), `GetFile`, `Audit`, and `Report`. It does
not import `internal/audit` or `internal/server` — it is a plain HTTP
client.

### 7.5 The demo — `demo/run.sh`

`demo/run.sh` is a scripted, self-checking walk-through: it starts `moatd`,
creates sandboxes over the HTTP API with the `dev-shell` template, runs a
real Go build in one, copies the binary out, and checks each isolation
property (own kernel, private rootfs, secret firewall, hash-chained audit).
It needs `/dev/kvm`, `curl`, `jq`, and `make demo-image` once. It exits
non-zero on the first failed check. `demo/README.md` has the step table.

---

## 8. Deploying `moatd`

### 8.1 Host requirements

- Linux on x86-64 with **nested or bare-metal KVM**. Bare metal for
  production (Equinix, Hetzner, AWS `*.metal`). GCP nested virtualization
  works for dev.
- `/dev/kvm` accessible to the `moatd` user.
- `firecracker` and `jailer` on `PATH`.
- An uncompressed `vmlinux` and a `rootfs.ext4` — from `make images`, or from
  the release tarball, or fetched by `install.sh`.
- cgroup v2 with a **delegated systemd subtree** for the rootless resource
  caps. Without delegation, `-cpu-max` / `-mem-max` / `-pids-max` need root.
- For networked sandboxes: `nft` on `PATH`, and `CAP_NET_ADMIN` +
  `CAP_SYS_ADMIN` (the systemd unit shows how to grant them).

Run `moat doctor` on the target host. It prints one line per check.

### 8.2 The installer

```
curl -fsSL https://raw.githubusercontent.com/M30W-M1X/moat/main/install.sh | sh
```

`install.sh` (POSIX sh) checks Linux / x86-64 / KVM, resolves the latest
GitHub release, downloads the tarball and `checksums.txt`, verifies SHA-256
(and cosign when present and configured), installs the binaries to
`/usr/local/bin` and the images plus `images.lock` and `sbom.cdx.json` to
`/var/lib/moat`, writes a starter `moatd.json` with a random token, and
installs `deploy/moatd.service`.

Flags: `--user`, `--prefix`, `--data`, `--version`, `--no-service`,
`--dry-run`.

### 8.3 The systemd unit

`deploy/moatd.service` runs `moatd -config /etc/moat/moatd.json`. It sets
`DeviceAllow=/dev/kvm rw`, `NoNewPrivileges`, `ProtectSystem=strict`,
`ProtectHome=read-only`, `PrivateTmp`, and a read-write path list of
`/var/lib/moat` and `/run/moat`. For networked sandboxes, add:

```
AmbientCapabilities=CAP_NET_ADMIN CAP_SYS_ADMIN
CapabilityBoundingSet=CAP_NET_ADMIN CAP_SYS_ADMIN
```

`systemctl enable --now moatd`.

### 8.4 Directory resolution

An installed binary with no flags finds its files through a fallback chain:

- **data** (`vmlinux`, `rootfs.ext4`, `images.lock`, the audit directory):
  `$MOAT_DATA` → `$XDG_DATA_HOME/moat` → `~/.local/share/moat` →
  `/var/lib/moat`. In a repo checkout it is `<repo>/build/artifacts`.
- **run** (per-VM sockets, pid files, scratch): `$MOAT_RUN` →
  `$XDG_RUNTIME_DIR/moat` → `/run/moat` → a temp directory.

### 8.5 Rootless versus root

- **Rootless** covers the common case: cold-boot sandboxes, exec, file I/O,
  resource caps (with systemd delegation), the audit log, the egress proxy in
  `HTTP_PROXY` mode, snapshots.
- **Root (or `unshare`)** is needed for the network namespace path
  (`-net`, `moatd` networked sandboxes), the transparent egress DNAT, the
  jailer (`-jail`), real-uplink NAT, and UFFD restore without
  `/dev/userfaultfd` access.

---

## 9. Configuration reference

### 9.1 `moatd` flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-addr` | `:8080` | API listen address |
| `-config FILE` | — | JSON file of templates, tokens, per-token policy, and defaults |
| `-tokens` | — | comma list `id:secret` (else `$MOAT_TOKENS`) |
| `-pool N` | `0` | warm, non-networked sandboxes to keep ready |
| `-global-max` / `-token-max` | `64` / `16` | concurrency caps (`0` = unlimited) |
| `-default-ttl` / `-max-ttl` | `15m` / `1h` | sandbox lifetime and its cap |
| `-kernel` / `-rootfs` | data dir defaults | default template image paths |
| `-vcpus` / `-mem` | `1` / `256` | default template size |
| `-cpu-max` / `-mem-max` / `-pids-max` | `0` | per-sandbox cgroup caps |
| `-disk-bw` / `-net-bw` | `0` | per-sandbox bandwidth caps, bytes/sec |
| `-net-pool` / `-uplink` | `10.62.0.0/16` / `none` | networked sandbox addressing and NAT parent |
| `-egress-addr` / `-egress-allow` / `-egress-deny` | `127.0.0.1:0` / — / — | egress proxy listener and domain policy |
| `-egress-transparent ADDR` | — | second listener plus the namespace DNAT |
| `-audit-dir` | data dir `/audit` | `""` = default, `"off"` disables |
| `-secret-scan` / `-secret-redact` | `true` / `false` | ingress firewall behaviour |
| `-image-verify` | `enforce` | `enforce` / `warn` / `off` |
| `-cosign-identity` / `-cosign-issuer` | — | verify `images.lock.sig` |
| `-json-log` | `true` | structured JSON request logs |

### 9.2 `moatd.json`

Top-level keys: `templates` (array), `tokens` (array), `defaults` (object),
`audit` (object), `image_verify`, `cosign_identity`, `cosign_issuer`.
Unknown keys are rejected.

- **template:** `name`, `kernel`, `rootfs`, `vcpus`, `mem_mib`, `cpu_max`,
  `mem_max_mib`, `pids_max`, `disk_bw`, `net_bw`. An empty `kernel` or
  `rootfs` inherits the built-in default.
- **token:** `id`, `secret`, `templates` (allowlist), `max_concurrent`,
  `max_ttl`, `deny_network`, `allow_secret_override`, `audit_read` (`""` or
  `"all"`).
- **defaults:** `global_max`, `token_max`, `default_ttl`, `max_ttl`, `pool`,
  `net_pool`, `uplink`.
- **audit:** `dir`, `max_file_mb`, `max_total_mb`, `max_age`, `fsync_batch`,
  `fsync_interval`.

A per-token policy is enforced in `handleCreate` before any boot. A blocked
request gets `403`. Sample: [`docs/moatd.example.json`](moatd.example.json).

### 9.3 Environment variables

`MOAT_TOKENS`, `MOAT_DATA`, `MOAT_RUN`, `MOAT_IMAGE_VERIFY`,
`MOAT_COSIGN_IDENTITY`, `MOAT_COSIGN_ISSUER`, `MOAT_ADDR` and `MOAT_TOKEN`
(the MCP server and the Go client). Precedence for the image settings is
flag, then env, then config file, then default.

---

## 10. Operations

### 10.1 Metrics — `GET /metrics`

Prometheus text. Series:

```
moat_sandboxes_active{template}
moat_sandboxes_created_total{template,source}
moat_sandbox_create_seconds{source}
moat_exec_total{outcome}
moat_file_bytes_total{direction}
moat_secret_findings_total{rule,action}
moat_quota_rejects_total
moat_pool_ready
moat_egress_requests_total{decision}
moat_http_requests_total{route,status}
moat_http_request_seconds{route}
moat_audit_events_total{type,result}
moat_audit_dropped_total
moat_audit_write_seconds{path}
moat_audit_queue_depth
```

### 10.2 The audit log

`moat audit tail -dir /var/lib/moat/audit` follows the log. `moat audit
query -dir ... -since -15m -type exec` filters offline. `moat audit report
-dir ... <sandbox-id>` prints one sandbox's rollup. `moat audit verify -dir
...` recomputes the chain and exits non-zero on a break.

Over the API: `moat audit tail -addr localhost:8080 -token <secret>` (the
token needs `audit_read: "all"` for an unfiltered pull).

### 10.3 Crash recovery

`moatd` runs `vm.Reconcile` at startup and kills orphan Firecracker
processes, one `orphan.reap` audit event per id. `moat reap` does the same by
hand. If `moatd` refuses to start with an audit-log error, run `moat audit
verify -dir <dir>`, move the named file aside, and restart.

### 10.4 Footguns

Every footgun that cost real debugging time is in the ClaudeVault "Gotchas"
note, numbered `G-1` .. `G-19`, in a symptom → cause → fix shape. Read it
before a deep change to the kernel args, the image pipeline, the network
namespace path, or the audit writer.

---

## 11. Limitations and roadmap

- **Cold-boot rootfs is read-write** (section 3.4, Gotcha `G-19`). Planned
  fix: attach the root drive read-only, or copy per sandbox.
- **A snapshot-templated warm pool** is not built. The pool cold-boots its
  members.
- **A gRPC northbound surface** alongside the REST API is planned.
- **A downloadable template registry** (`moat template pull python`) is a
  deferred CDN job.
- **`make images-verify` in CI** — the repo has no CI yet. The target is the
  exact check a CI job would run.
- **Signing `images.lock` in an automated release** is manual today.
- **GPU workloads** are out of scope for the Firecracker path (`DD-11`). A
  parallel gVisor or Cloud Hypervisor backend would keep the VM-driver
  interface abstract.

---

## 12. Contributing

- **Style.** All code comments and all log and error strings follow
  ASD-STE100 (Simplified Technical English): active voice, one instruction
  per sentence, ≤ 20 words for instructions and ≤ 25 for descriptions, no
  semicolons, no phrasal verbs ("start" and "stop", never "boot" or "tear
  down"), simple tenses. Keep one verb for one action.
- **Knowledge systems.** Durable decisions and how-we-work facts go into the
  agentcairn vault. Full design and reference write-ups go into
  `~/Documents/ClaudeVault/projects/microvm-controller/`. Every footgun that
  costs real debugging time gets a numbered `G-N` entry in the "Gotchas"
  note before you move on.
- **Non-trivial work** goes through a spec in
  `docs/superpowers/specs/` and a plan in `docs/superpowers/plans/` before
  implementation. The audit event system and the reproducible image builds
  both followed that flow — read those files for the pattern.
- **No new Go module dependency** without a strong reason. The secret scanner
  vendors a rule file rather than import the Gitleaks module (200+
  dependencies). The SBOM generator hand-marshals CycloneDX.
- **Tests.** Unit tests are the gate. Add an integration test for anything
  that touches the boot path, the network, or the API. Add a `-race` test for
  anything that touches the audit writer or a shared setter.
