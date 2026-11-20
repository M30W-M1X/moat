# moatd HTTP API

`moatd` is the self-hosted control plane for Moat. It gives agent products, CI runners, and coding assistants disposable Firecracker microVMs over plain HTTP/JSON or a Unix domain socket — with bearer-token multi-tenancy, per-template warm pools, an ingress secret firewall, strict domain-filtered egress, native TLS/mTLS, Prometheus metrics, and a hash-chained audit trail.

Zero external accounts. Zero vendor lock-in. Every isolation feature ships under Apache-2.0.

## Run

```sh
# Quick start from flags
MOAT_TOKENS="ci:s3cr3t,dev:hunter2" moatd -addr 127.0.0.1:8080 -pool 4

# Production start from config (TLS or Unix socket, per-token policies, audit log)
moatd -config /etc/moat/moatd.json
```

Flags: `-addr`, `-socket`, `-tls-cert`, `-tls-key`, `-tls-client-ca`,
`-config FILE`, `-tokens id:secret,...` (or `$MOAT_TOKENS`),
`-pool N`, `-global-max`, `-token-max`, `-default-ttl`, `-max-ttl`, `-kernel`,
`-rootfs`, `-vcpus`, `-mem`, `-cpu-max`, `-mem-max`, `-pids-max`, `-disk-bw`,
`-net-bw`, `-jail`, `-jail-uid`, `-jail-gid`, `-net-pool`, `-uplink`,
`-egress-addr`, `-egress-allow`, `-egress-deny`, `-egress-transparent`,
`-strict-egress`, `-audit-dir`, `-audit-webhook`, `-image-verify`,
`-cosign-identity`, `-cosign-issuer`, `-secret-scan` (default on),
`-secret-redact`, `-json-log`.

Pass `-addr ""` with `-socket /run/moat/moatd.sock` to listen exclusively on a local Unix socket (`0660`).

## Config file (`moatd.json`)

`-config FILE` defines templates, bearer tokens, per-token policies, transport security, and audit settings. Unknown keys are rejected at startup. See [`docs/moatd.example.json`](moatd.example.json) for a full working example.

```json
{
  "image_verify": "enforce",
  "socket": "/run/moat/moatd.sock",
  "templates": [
    {
      "name": "default",
      "vcpus": 1,
      "mem_mib": 256,
      "cpu_max": 0.5,
      "mem_max_mib": 512,
      "pids_max": 256,
      "pool": 4,
      "read_only_root": true,
      "scratch_mib": 512
    }
  ],
  "tokens": [
    {
      "id": "ci",
      "secret": "replace-me",
      "templates": ["default"],
      "max_concurrent": 8,
      "max_ttl": "30m",
      "egress_allow": ["pypi.org", "github.com"]
    }
  ],
  "defaults": {
    "global_max": 64,
    "token_max": 16,
    "default_ttl": "15m",
    "max_ttl": "1h",
    "jail": true,
    "strict_egress": true
  },
  "audit": {
    "dir": "/var/lib/moat/audit",
    "webhook_url": "https://siem.internal/moat/events"
  }
}
```

### Key capabilities in `moatd.json`

- **Per-template pools & snapshots:** set `"pool"` per template, and optionally `"snapshot_dir"` + `"use_uffd": true` to warm pool members from snapshots with automatic guest clock resync.
- **Read-only rootfs + overlayfs:** set `"read_only_root": true` and `"scratch_mib"` so sandboxes mount the shared template read-only and write only to a sparse per-VM ext4 scratch disk.
- **Per-token policy:** restrict `"templates"`, `"max_concurrent"`, `"max_ttl"`, `"deny_network"`, `"allow_secret_override"`, `"audit_read": "all"`, and per-token `"egress_allow"` / `"egress_deny"` (global deny rules always win over per-token allow rules).
- **VMM jailer & strict egress:** `"defaults.jail"` (`jail_uid`, `jail_gid`) runs Firecracker under the jailer; `"defaults.strict_egress"` sets the sandbox `nftables` forward policy to `drop` so only proxy traffic and configured DNS pass.
- **Transport security:** `"socket"` opens a `0660` Unix socket; `"tls_cert"` + `"tls_key"` enable TLS 1.2+; `"tls_client_ca"` enforces mutual TLS (`RequireAndVerifyClientCert`).
- **SIEM forwarding:** `"audit.webhook_url"` (or `-audit-webhook`) forwards every written audit event asynchronously via bounded-queue JSON POST without blocking API requests.

## Secret firewall

On by default. Every file, environment value, and stdin crossing into a sandbox is scanned with the vendored Gitleaks rule set (regex + keyword + Shannon entropy). A match returns `409` and zero bytes reach the guest:

```json
{
  "error": "secret detected in /work/.env — not sent",
  "hint": "remove the credential, or resend with allow_secrets if your token permits it",
  "findings": [
    { "rule": "aws-secret-key", "line": 3, "secret": "aZ****(40 chars)", "entropy": 5.1 }
  ]
}
```

To override, the caller's token needs `"allow_secret_override": true` and the request must pass `"allow_secrets": true` (exec) or `?allow_secrets=true` (file `PUT`). Every override is synchronously written to the audit log; if the audit write fails, the request fails closed (`503`). Start `moatd` with `-secret-redact` to replace findings with a marker instead of blocking, or `-secret-scan=false` to disable.

## Auth

Every `/v1/...` route requires `Authorization: Bearer <secret>` (checked in constant time). Callers see only sandboxes owned by their token. `/healthz` and `/metrics` are unauthenticated.

## Routes

### `POST /v1/sandboxes`

Create a sandbox. Body (all fields optional, max 1 MiB):

```json
{ "template": "default", "networked": false, "ttl_seconds": 300 }
```

`201 Created`:

```json
{
  "id": "sb-7f3a19c4",
  "template": "default",
  "created_at": "2026-08-30T18:10:00Z",
  "expires_at": "2026-08-30T18:15:00Z",
  "networked": false,
  "guest_ip": ""
}
```

Errors: `403` (token policy forbids template or networking), `413` (oversized body), `429` (per-token or global quota reached), `503` (audit write failed — sandbox rolled back).

### `GET /v1/sandboxes`

`200` with `{ "sandboxes": [ <sandbox>, ... ] }` — active sandboxes owned by the caller's token.

### `GET /v1/sandboxes/{id}` · `DELETE /v1/sandboxes/{id}`

Inspect or destroy (`204 No Content`). Sandboxes are also reaped automatically at `expires_at`.

### `POST /v1/sandboxes/{id}/exec`

Run one process inside the guest. Body:

```json
{
  "argv": ["sh", "-c", "echo hi"],
  "env": { "K": "V" },
  "dir": "/work",
  "stdin": "<base64>",
  "tty": false,
  "timeout_ms": 30000,
  "allow_secrets": false
}
```

`409` when an env value or stdin trips the secret firewall; `413` when the body exceeds the upload cap.

Streams `application/x-ndjson`, one JSON object per line:

```json
{"stream":"stdout","data":"<base64>"}
{"stream":"stderr","data":"<base64>"}
{"stream":"exit","code":0,"signal":0,"timed_out":false}
```

For networked sandboxes, `moatd` injects `HTTP_PROXY` / `HTTPS_PROXY` pointing at the egress proxy and prevents caller overrides. With `-egress-transparent ADDR`, `nftables` DNATs guest TCP 80/443 directly to the transparent SNI/Host-filtering proxy (no TLS termination) and drops arbitrary outbound ports under strict egress.

### `PUT /v1/sandboxes/{id}/files/{path}`

Write the raw request body to `/{path}` in the guest. `200` with `{ "bytes_written": N, "sha256": "..." }`. `409` on a secret hit (`?allow_secrets=true` to override if permitted). `413` when larger than the upload cap (64 MiB default).

### `GET /v1/sandboxes/{id}/files/{path}`

Stream `/{path}` back as `application/octet-stream`; the `X-Sha256` trailer holds the digest. `404` when absent.

## Audit log

### `GET /v1/audit`

Streams newline-delimited JSON (`application/x-ndjson`) in `seq` order.

| Query param | Meaning |
| --- | --- |
| `sandbox` | Exact sandbox ID |
| `token` | Exact token ID (requires `audit_read: "all"`) |
| `type` | Repeatable exact event type (`sandbox.create`, `exec`, `secret.block`, `egress.deny`, ...) |
| `since`, `until` | RFC3339 timestamp or negative Go duration (e.g. `-15m`) |
| `limit` | Max events (default 1000, cap 100000) |
| `order` | `asc` (default) or `desc` |

Events record metadata only — never file contents, env values, or command output. Every event carries `prev` and `hash` (SHA-256 of canonical JSON). On startup `moatd` verifies the log and refuses to start on tampering. Verify any time with `moat audit verify`.

`daemon.start` records an `images` provenance array from `images.lock`. In default `enforce` mode, any template failing digest or Cosign signature verification is dropped before serving.

### `GET /v1/sandboxes/{id}/report`

Returns a JSON rollup of one sandbox's lifetime: exec counts, bytes in/out, file hashes, secret-firewall events, egress allow/deny hosts, and chain verification status. Works even after the sandbox is destroyed.

## Observability

- `GET /metrics` — Prometheus exposition including `moat_sandboxes_active{template}`, `moat_sandboxes_created_total{template,source}`, `moat_sandbox_create_seconds{source}`, `moat_exec_total{outcome}`, `moat_file_bytes_total{direction}`, `moat_secret_findings_total{rule,action}`, `moat_quota_rejects_total`, `moat_pool_ready`, `moat_http_requests_total{route,status}`, `moat_egress_requests_total{decision}`, and audit writer counters.
- Request logs emit one structured JSON line per request (`method`, `route`, `status`, `dur_ms`).

## curl walk-through

```sh
T='Authorization: Bearer s3cr3t'
ID=$(curl -s -XPOST -H "$T" -d '{"ttl_seconds":120}' http://127.0.0.1:8080/v1/sandboxes | jq -r .id)
curl -s -XPOST -H "$T" -d '{"argv":["uname","-a"]}' http://127.0.0.1:8080/v1/sandboxes/$ID/exec
curl -s -XPUT  -H "$T" --data-binary @./script.py http://127.0.0.1:8080/v1/sandboxes/$ID/files/tmp/s.py
curl -s -XPOST -H "$T" -d '{"argv":["python3","/tmp/s.py"]}' http://127.0.0.1:8080/v1/sandboxes/$ID/exec
curl -s -XGET  -H "$T" http://127.0.0.1:8080/v1/sandboxes/$ID/report | jq .
curl -s -XDELETE -H "$T" http://127.0.0.1:8080/v1/sandboxes/$ID
```
