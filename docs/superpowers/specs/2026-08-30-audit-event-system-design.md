# Audit Event System — Design

Status: approved for implementation planning
Date: 2026-08-30
Milestone: M2 "Production & fleet" → first slice of "Observability & trust"
Author: paired design session

## 1. Summary

`moatd` gains a durable, tamper-evident **audit log** of every boundary
crossing a sandbox makes: creation and destruction, command execution, file
transfer in and out, ingress secret-firewall decisions, egress domain
decisions, quota rejections, and orphan reaping. Events are appended as
newline-delimited JSON to a rotated set of files, each entry hash-chained to
the previous one. A read API and a `moat audit` CLI expose the log with
per-token scoping, plus a per-sandbox "what did this sandbox do" report and a
chain-integrity `verify`.

This is the foundation of the "Observability & trust" milestone. The
multi-node fleet control plane (later) will consume the same event model.

### Goals

- Every boundary crossing produces one durable, ordered, metadata-only event.
- The log is tamper-evident: any edit, deletion, or reordering is detectable.
- Boundary events (create / destroy / secret block / egress deny) are
  persisted **before** the corresponding API response, so "the API said 201"
  implies "the create event is on disk".
- An operator can answer "what did sandbox `sb-X` do" and "what has token `T`
  been doing" without re-running anything and without the audit log itself
  ever holding a secret, a payload, or program output.
- No new dependency in the `moatd` module. Files only. Nothing leaves the
  machine unless the operator ships the files.

### Non-goals (v1)

- The `moat run` one-shot CLI path (direct VM boot, no daemon) writes no
  audit events.
- No external sink (syslog, webhook, object store). A pluggable sink
  interface is sketched but not built.
- No cryptographic signing or external notary — hash chain only.
- No web console / UI.
- SBOM and reproducible image builds are a separate follow-up spec.

## 2. Event model

### 2.1 Envelope

Every event is a single JSON object on one line:

```json
{
  "seq": 42,
  "ts": "2026-08-30T18:14:07.913482Z",
  "type": "exec",
  "sandbox": "sb-7f3a19c4",
  "token": "ci",
  "attrs": { "...": "type-specific, metadata only" },
  "prev": "a91c4f2b…64hex",
  "hash": "7d2e0813…64hex"
}
```

| Field | Type | Notes |
|---|---|---|
| `seq` | uint64 | Monotonic per recorder. Starts at 1. Survives rotation and restart. Gap ⇒ lost events. |
| `ts` | RFC3339Nano UTC | Wall clock at record time. |
| `type` | string | Dotted, from the closed set in §2.3. |
| `sandbox` | string | Sandbox id, or `""` for daemon-level events. |
| `token` | string | Token **id** (never the secret). `""` for daemon-level events. |
| `attrs` | object | Type-specific. Metadata only (see §2.4). |
| `prev` | 64 hex | `hash` of the previous event. All-zero for `seq == 1` and for the first event after a retention prune removed the real predecessor. |
| `hash` | 64 hex | sha256 of this event's **canonical form** (§2.2). |

### 2.2 Canonical form and the hash chain

The canonical form of an event is its JSON object with:

- keys sorted lexicographically at every level,
- the `hash` field omitted,
- no insignificant whitespace,
- integers as integers, strings as-is (Go `encoding/json` with
  `SetEscapeHTML(false)` over a `map[string]any` built from an ordered walk, or
  a small canonical marshaller — implementation detail for the plan).

`hash = hex(sha256(canonical_bytes))`. `prev` is copied from the previous
event's `hash`. The chain continues unbroken across file rotation: the first
event in a new file carries the `prev` of the last event in the previous file.

`moat audit verify` recomputes every `hash` and checks every `prev` link over
the retained files. Failures reported: `hash mismatch at seq N`, `prev break
at seq N` (expected content changed), `seq gap N..M`, `out-of-order ts at
seq N` (advisory). A `prev` that points at a pruned entry is reported as
`chain starts at seq N (earlier entries pruned)` — not a failure.

### 2.3 Event types (closed set for v1)

| `type` | When | Path |
|---|---|---|
| `daemon.start` | `moatd` begins serving | sync |
| `daemon.stop` | graceful shutdown begins | sync |
| `sandbox.create` | a sandbox is handed to a caller | sync |
| `sandbox.destroy` | a sandbox is torn down | sync |
| `exec` | an `exec` call finishes (or errors) | async |
| `file.put` | `PUT .../files/...` completes | async |
| `file.get` | `GET .../files/...` completes | async |
| `secret.block` | ingress content blocked (409) | sync |
| `secret.allow` | flagged ingress content passed via `allow_secrets` | sync |
| `secret.redact` | flagged ingress content redacted and passed | sync |
| `egress.deny` | egress proxy refused a destination | sync |
| `egress.allow` | egress proxy permitted a destination | async |
| `quota.reject` | create refused, quota full | sync |
| `orphan.reap` | daemon killed a leftover Firecracker process | async |

"sync" = appended and `fsync`'d before the triggering API response is sent;
its failure fails the request (§3.1). "async" = enqueued, batch-flushed;
its failure never fails a request but increments `moat_audit_dropped_total`.

### 2.4 `attrs` per type (metadata only — never a payload, env value, file body, or output)

- `daemon.start`: `version`, `templates` (`[]string`), `pool_size`,
  `net_pool`, `secret_scan` (bool), `egress` (bool), `config_sha256`
  (sha256 of the loaded `moatd.json` bytes, or `""` when started from flags
  only).
- `daemon.stop`: `reason` (`signal`|`api`), `uptime_s`.
- `sandbox.create`: `template`, `source` (`pool`|`cold`), `networked`,
  `ttl_s`, `guest_ip` (or `""`).
- `sandbox.destroy`: `reason` (`deleted`|`ttl`|`idle`|`shutdown`|`orphan`),
  `lifetime_s`.
- `exec`: `argv` (`[]string`, full — it is the command, not data; capped at
  4096 bytes total, over-long tail replaced with `"…"`), `env_keys`
  (`[]string`, sorted), `dir`, `tty` (bool), `exit`, `signal`, `timed_out`
  (bool), `dur_ms`, `error` (string, set only when the exec call itself
  failed).
- `file.put`: `path`, `bytes`, `sha256`, `scan` (`clean`|`redact`|`allow`|
  `block`). A `block` here is also emitted as `secret.block`; `file.put` with
  `scan:block` records that the write did not happen.
- `file.get`: `path`, `bytes`, `sha256`.
- `secret.block` / `secret.allow` / `secret.redact`: `target`
  (`env:KEY`|`stdin`|`<guest path>`), `rules` (`[]string`, the rule ids that
  fired), `findings` (count). The matched value is never recorded.
- `egress.allow` / `egress.deny`: `host`, `port`, `method` (for plain HTTP)
  or `"CONNECT"`, `matched_rule` (the allow/deny pattern that decided it, or
  `""`), `via` (`proxy`|`transparent`). The envelope `sandbox` and `token`
  are set from the client-IP resolver (§4.3); when it cannot resolve the
  client IP the envelope carries `sandbox:""`, `token:""` and `attrs` adds
  `client_ip`.
- `quota.reject`: `scope` (`global`|`token`), `limit`, `template`.
- `orphan.reap`: `pid`, `sandbox` (best-effort from the runtime dir name).

## 3. Write path

### 3.1 `audit.Recorder`

```go
package audit

type Event struct {
    Seq     uint64
    TS      time.Time
    Type    string
    Sandbox string
    Token   string
    Attrs   map[string]any
    Prev    string
    Hash    string
}

type Recorder struct { /* file handle, mutex, seq, chain tip, async chan, cfg */ }

// RecordSync assigns seq + chain fields, appends, and fsyncs before returning.
// A non-nil error means the event is NOT on disk; the caller must treat it as
// fatal for the request in progress.
func (r *Recorder) RecordSync(ctx context.Context, e Event) error

// Record enqueues e on the async buffer. Never blocks. On a full buffer it
// drops e, increments the drop metric, and logs at error level.
func (r *Recorder) Record(e Event)

// Close flushes the async buffer and fsyncs. Called during Shutdown, after the
// daemon.stop event is recorded synchronously.
func (r *Recorder) Close() error
```

- One `sync.Mutex` guards `seq`, the chain tip, and the file handle. Both
  `RecordSync` and the async writer goroutine take it, so `seq` is always
  dense and the chain is always consistent regardless of path.
- `RecordSync`: lock → `seq++`, set `TS`, `Prev` = tip, compute `Hash`,
  write line, `f.Sync()`, tip = `Hash` → unlock. Return any error.
- Async: `Record` does a non-blocking send on `chan Event` (cap 8192). One
  writer goroutine drains it, taking the same lock per event, and calls
  `f.Sync()` after `fsync_batch` events or `fsync_interval`, whichever comes
  first. On send failure (full): `moat_audit_dropped_total.Inc()` + error log.
- If `dir` is `""` the Recorder is a no-op (`RecordSync` returns nil,
  `Record` discards). `server` and `cmd/moatd` always hold a non-nil
  `*Recorder`; "disabled" is the no-op recorder.

### 3.2 Rotation and retention

- Current file: `audit-<YYYY-MM-DD>.jsonl`, or `audit-<YYYY-MM-DD>.<n>.jsonl`
  when the same day rolls past `max_file_mb`.
- Before each append the writer checks size and date; on a roll it opens the
  next file and writes the first line with `prev` = the last chain tip
  (unbroken across files).
- After a roll, prune: delete whole files whose newest `ts` is older than
  `max_age`, then delete oldest files while total bytes exceed
  `max_total_mb`. The current file is never pruned.
- `verify` and the readers operate over whatever files remain.

### 3.3 Startup

On `New`, the Recorder scans the retained files newest-first to recover:

- the highest `seq` and the chain tip (for continuation),
- an in-memory **index**: `map[sandboxID][]locator` and
  `map[tokenID][]locator` where `locator = {file index, byte offset}`, plus
  per-sandbox first/last `ts`. The index holds offsets, not events, to bound
  memory (~tens of bytes per event).
- If the newest file's last line is truncated (crash mid-write), it is
  discarded to the previous newline and logged; the chain resumes from the
  last intact entry.

A full rescan on start is acceptable within the retention cap (default 2 GiB
⇒ low seconds). Documented; revisit with a sidecar index file only if it
becomes a problem.

## 4. Read surface

### 4.1 HTTP API

**`GET /v1/audit`** — query the log. Response: `application/x-ndjson`, one
event per line, `seq` order (or reverse with `order=desc`).

| Param | Meaning |
|---|---|
| `sandbox` | exact sandbox id |
| `token` | exact token id (only honored for `audit_read:"all"` callers) |
| `type` | repeatable; exact event type |
| `since`, `until` | RFC3339 or `-15m` style relative; `ts` window |
| `limit` | max events (default 1000, hard cap 100000) |
| `order` | `asc` (default) or `desc` |

Authorization:

- Default token: results are filtered to events whose `sandbox` was created
  by this token (join via the index's sandbox→token map) **or** whose
  `token` equals the caller. A `sandbox=` filter for a sandbox the caller
  does not own returns an empty stream (not 403, to avoid id probing).
- `audit_read:"all"` token: no filtering; `token=` filter honored.

**`GET /v1/sandboxes/{id}/report`** — JSON rollup for one sandbox. A caller
that neither owns the sandbox nor holds `audit_read:"all"` gets `404 "no such
sandbox"` (same as `mustOwn` today, so the endpoint does not confirm an id
exists). The sandbox need not still be live — the report is built from the
audit files, so it works after destruction until the events are pruned.
Shape:

```json
{
  "sandbox": "sb-7f3a19c4",
  "token": "ci",
  "template": "python",
  "created_at": "…", "destroyed_at": "…", "destroy_reason": "ttl",
  "lifetime_s": 903,
  "networked": false, "guest_ip": "",
  "counts": { "exec": 3, "file.put": 2, "file.get": 1, "egress.allow": 5, "egress.deny": 1, "secret.block": 1 },
  "exec": { "n": 3, "nonzero": 1, "errors": 0, "total_dur_ms": 4120 },
  "bytes": { "in": 824, "out": 1290 },
  "files": [ { "dir": "in", "path": "/work/task.py", "bytes": 412, "sha256": "9f2c…", "scan": "clean" } ],
  "secret_events": [ { "type": "secret.block", "target": "env:AWS_SECRET_ACCESS_KEY", "rules": ["aws-access-token"] } ],
  "egress": { "allow": ["pypi.org:443", "files.pythonhosted.org:443"], "deny": ["169.254.169.254:80"] },
  "chain": { "first_seq": 40, "last_seq": 61, "verified": true }
}
```

Built by scanning that sandbox's indexed offsets only.

### 4.2 CLI — `moat audit`

| Command | Behaviour |
|---|---|
| `moat audit tail [-f] [--sandbox X] [--type T]` | recent events; `-f` polls `GET /v1/audit?order=desc&limit=…` and prints new ones |
| `moat audit query --since --until --sandbox --token --type [--json]` | one-shot query; table or NDJSON |
| `moat audit report <sandbox-id> [--json]` | the §4.1 rollup as a table or JSON |
| `moat audit verify [--since]` | chain integrity; over the API needs `audit_read:"all"` |

All subcommands accept `--dir /var/lib/moat/audit` to bypass the API and read
the files directly via `audit.Reader` (works when the daemon is down; the
primary path for forensic `verify`). `--addr` / `--token` /
`$MOAT_ADDR` / `$MOAT_TOKEN` as elsewhere.

### 4.3 Client and integration points

- `internal/moatapi`: `Audit(ctx, AuditFilter) ([]Event, error)` (NDJSON
  assembled, like `Exec`) and `Report(ctx, id) (Report, error)`.
- `internal/audit.Reader`: `Query(files, Filter) iter.Seq2[Event, error]`,
  `Report(files, sandboxID) (Report, error)`, `Verify(files) (VerifyResult,
  error)`. Used by both the daemon (over its live files) and the CLI
  `--dir` path.
- **Egress → sandbox resolution.** `internal/egress.Proxy` identifies a
  client only by source IP. `server` knows each networked sandbox's
  `netconf.Plan` (`VethHostIP` / `GuestIP`). The server passes the Proxy an
  `Identify func(clientIP netip.Addr) (sandboxID, tokenID string, ok bool)`
  closure at construction. On `ok == false` the event is recorded with
  `sandbox:""`, `token:""`, `attrs.client_ip`. The Proxy holds an optional
  `*audit.Recorder`; when nil it keeps today's behaviour (log + metric only).

## 5. Configuration

`moatd.json`:

```json
"audit": {
  "dir": "/var/lib/moat/audit",
  "max_file_mb": 128,
  "max_total_mb": 2048,
  "max_age": "720h",
  "fsync_batch": 64,
  "fsync_interval": "200ms"
}
```

- Absent `audit` block ⇒ enabled at `<state>/audit` (`config.RunRoot()` /
  data dir sibling). Audit-on-by-default is deliberate for a trust feature.
- `"dir": ""` ⇒ disabled (no-op recorder).
- `moatd -audit-dir PATH` overrides `dir`; `-audit-dir ""` disables.
- Per token in `moatd.json`: `"audit_read": "all"` (string; only `"all"` is
  meaningful; absent/empty = own events only). Maps to
  `server.TokenPolicy.AuditRead`, mirroring `allow_secret_override`.
- `cmd/moatd/config.go`: validate `max_*` > 0, durations parseable, `dir`
  writable (create it, write+unlink a probe file) at load; fail fast.

## 6. Metrics (added to `internal/server/metrics.go`)

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `moat_audit_events_total` | counter | `type`, `result` (`ok`/`error`) | events recorded |
| `moat_audit_dropped_total` | counter | — | async events dropped (buffer full or write error). Alarm on any increase. |
| `moat_audit_write_seconds` | histogram | `path` (`sync`/`async`) | append+fsync latency |
| `moat_audit_queue_depth` | gauge | — | current async buffer occupancy |

## 7. Testing

### `internal/audit` (unit)

- Canonical hashing is deterministic across map iteration order and process
  runs; known vector.
- Chain continues correctly across a forced rotation.
- `Verify` catches: an edited attr, a deleted middle line, two swapped lines,
  a truncated last line, a `seq` gap. A pruned prefix reports
  `chain starts at seq N`, not a failure.
- Index rebuild from a multi-file set matches a linear scan.
- Retention prune removes the right files by age and by total size; never the
  current file.
- Async: a full buffer drops and increments the metric; after drain, `seq`
  is still dense and the chain still verifies.
- Concurrency: N goroutines mixing `RecordSync` and `Record` produce a
  gap-free, monotonic, verifiable log.
- `"dir": ""` no-op recorder: `RecordSync` returns nil, `Record` discards,
  `Verify` over an empty set is clean.

### `internal/server` (integration, `-tags integration`)

- A `create → put(secret) → exec → get → delete` flow writes exactly the
  expected ordered event types to the audit dir, with the expected `attrs`
  keys and no forbidden keys (assert no `stdout`, `stderr`, `content`,
  `value`).
- `GET /v1/audit`: a `ci` token sees only its own sandbox's events; a
  `secops` token with `audit_read:"all"` sees both tokens' events and can
  filter by `token=`.
- `GET /v1/sandboxes/{id}/report`: shape and counts for a known flow;
  403-equivalent (empty / not-found) for a non-owner without `all`.
- Making the audit dir unwritable before a `create` ⇒ 503 and the sandbox is
  rolled back (no leaked Firecracker process, `moat_sandboxes_active`
  unchanged).
- `daemon.start` / `daemon.stop` bracket the run.

### `cmd/moat` (integration smoke)

- `moat audit query --json`, `moat audit report <id>`, `moat audit verify`
  against a live daemon; `moat audit verify --dir` against the files with the
  daemon stopped.

## 8. Documentation

- `docs/api.md`: `GET /v1/audit`, `GET /v1/sandboxes/{id}/report`,
  `audit_read` token policy.
- `docs/moatd.example.json`: an `audit` block and an `secops` token with
  `audit_read: "all"`.
- `README.md`: audit-log bullet under P5/M2.
- `SKILL.md`: a line telling the agent that a blocked action and its own
  sandbox activity are auditable (no behaviour change).
- ClaudeVault `Build Log.md` + `Gotchas.md` as work lands; agentcairn note.

## 9. Rollout

Single change set on `main` behind the on-by-default config. No migration —
the first `moatd` start with the new binary creates the audit dir and begins
at `seq 1`. Turning it off is `"dir": ""`. No wire-format or API breakage
for existing clients (`/v1/audit` and the report endpoint are additive).

## 10. Deferred (tracked, not in this spec)

- Pluggable sink interface (`type Sink interface { Emit(Event) }`) for
  syslog / webhook / object-store fan-out.
- Optional SQLite-backed indexer that tails the JSONL for fast historical
  queries on large deployments.
- Signing / external notary / transparency-log submission.
- Audit events from the `moat run` one-shot path.
- SBOM + reproducible image builds (separate "Observability & trust" spec).
- Fleet control plane consuming this event model across nodes.
