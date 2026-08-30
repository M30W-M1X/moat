# moatd HTTP API

`moatd` is the northbound daemon. It serves a JSON API for sandbox lifecycle,
command execution, and file transfer.

## Run

```sh
MOAT_TOKENS="ci:s3cr3t,dev:hunter2" moatd -addr :8080 -pool 4
```

Flags: `-addr`, `-config FILE`, `-tokens id:secret,...` (or `$MOAT_TOKENS`),
`-pool N`, `-global-max`, `-token-max`, `-default-ttl`, `-max-ttl`, `-kernel`,
`-rootfs`, `-vcpus`, `-mem`, `-cpu-max`, `-mem-max`, `-pids-max`, `-disk-bw`,
`-net-bw`, `-net-pool`, `-uplink`, `-egress-addr`, `-egress-allow`,
`-egress-deny`, `-egress-transparent`, `-secret-scan` (default on),
`-secret-redact`, `-json-log`.

## Config file

`-config FILE` reads a JSON file that defines the templates, the tokens, and a
per-token policy. It replaces `-tokens` and the single flag-built template. A
non-zero value in the file's `defaults` block overrides the matching flag. See
`docs/moatd.example.json`.

```json
{
  "templates": [
    { "name": "default", "vcpus": 1, "mem_mib": 256, "cpu_max": 0.5 }
  ],
  "tokens": [
    { "id": "ci", "secret": "…", "templates": ["default"],
      "max_concurrent": 8, "max_ttl": "30m", "deny_network": true }
  ],
  "defaults": { "global_max": 64, "pool": 4, "default_ttl": "15m" }
}
```

A token with an empty `templates` list may use any template. `max_concurrent`
`0` falls back to `token_max`; `max_ttl` empty falls back to `max_ttl`. Add
`"allow_secret_override": true` to a token to let its requests carry
secret-flagged content (see below).

## Secret firewall

On by default. Every file, environment value, and stdin crossing into a
sandbox is scanned with the Gitleaks rule set (regex + keyword + Shannon
entropy). A match returns `409` and the content is not sent:

```json
{ "error": "secret detected in /work/.env — not sent",
  "hint": "remove the credential, or resend with allow_secrets if your token permits it",
  "findings": [ { "rule": "aws-secret-key", "line": 3, "secret": "aZ****(40 chars)", "entropy": 5.1 } ] }
```

To override, the caller's token needs `allow_secret_override` and the request
must ask for it: `"allow_secrets": true` in the exec body, or
`?allow_secrets=true` on the file `PUT`. Overrides are logged. Start `moatd`
with `-secret-redact` to replace each secret with a marker and let the rest of
the content through instead of blocking. `-secret-scan=false` turns the
firewall off.

## Auth

Every `/v1/...` route needs `Authorization: Bearer <secret>`. A caller sees only
the sandboxes created with its own token. `/healthz` and `/metrics` are open.

## Routes

### `POST /v1/sandboxes`

Create a sandbox. Body (all optional):

```json
{ "template": "default", "networked": false, "ttl_seconds": 300 }
```

`201` with:

```json
{ "id": "sb-…", "template": "default",
  "created_at": "…", "expires_at": "…",
  "networked": false, "guest_ip": "" }
```

`429` when the per-token or global concurrency quota is full. `403` when a
token policy forbids the template or a networked sandbox.

### `GET /v1/sandboxes`

`200` with `{ "sandboxes": [ <sandbox>, … ] }` — the caller's sandboxes.

### `GET /v1/sandboxes/{id}` · `DELETE /v1/sandboxes/{id}`

Status, or destroy (`204`). Sandboxes are also destroyed automatically at
`expires_at`.

### `POST /v1/sandboxes/{id}/exec`

Run one process. Body:

```json
{ "argv": ["sh","-c","echo hi"], "env": {"K":"V"}, "dir": "/",
  "stdin": "<base64>", "tty": false, "timeout_ms": 0, "allow_secrets": false }
```

`409` when an env value or stdin holds a credential (see Secret firewall); the
command does not run.

Response is `application/x-ndjson`, one JSON object per line:

```json
{"stream":"stdout","data":"<base64>"}
{"stream":"stderr","data":"<base64>"}
{"stream":"exit","code":0,"signal":0,"timed_out":false}
```

For a networked sandbox the server sets `HTTP_PROXY` / `HTTPS_PROXY` in the
process environment, pointing at the egress proxy, and a caller cannot override
them. With `-egress-transparent ADDR` the daemon also adds an nftables redirect
in the sandbox namespace that forces guest TCP 80 and 443 through the proxy, so
a client that ignores the proxy variables still cannot bypass the domain policy.
The transparent path filters on the HTTP Host header and the TLS SNI and does
not terminate TLS.

### `PUT /v1/sandboxes/{id}/files/{path}`

Write the raw request body to `/{path}` in the guest. `200` with
`{ "bytes_written": N, "sha256": "…" }`. `409` when the body holds a credential
(see Secret firewall); add `?allow_secrets=true` to override if the token
permits it. `413` when the body is larger than the scan limit (64 MiB).

### `GET /v1/sandboxes/{id}/files/{path}`

Stream `/{path}` back as `application/octet-stream`; the `X-Sha256` trailer holds
the digest. `404` when the file does not exist.

## Audit log

### `GET /v1/audit`

Newline-delimited JSON (`application/x-ndjson`), one event per line, in `seq`
order.

| Query param | Meaning |
| --- | --- |
| `sandbox` | exact sandbox id |
| `token` | exact token id (only for a token with `audit_read: "all"`) |
| `type` | repeatable; exact event type |
| `since`, `until` | RFC3339, or a Go duration with a leading `-` (e.g. `-15m`) |
| `limit` | max events (default 1000, cap 100000) |
| `order` | `asc` (default) or `desc` |

A normal token sees only events for sandboxes it created, plus events tagged
with its own token id. A token with `audit_read: "all"` sees every event.

Event shape:

```json
{"seq":42,"ts":"2026-08-30T18:14:07.913482Z","type":"exec","sandbox":"sb-7f3a19c4",
 "token":"ci","attrs":{"argv":["python","/work/task.py"],"exit":0,"dur_ms":1180},
 "prev":"a91c4f…","hash":"7d2e08…"}
```

Events are metadata only. No payload, environment value, file body, or command
output is ever recorded. Each `hash` is the sha256 of the event's canonical
form; `prev` chains to the previous event. Verify the chain with
`moat audit verify`.

On startup, `moatd` verifies the audit log file and will not start if a
complete (non-final) line is corrupt or tampered. Run `moat audit verify -dir
<auditdir>` to find the bad file, then move it aside and restart.

### `GET /v1/sandboxes/{id}/report`

A JSON rollup of one sandbox's activity, built from the audit log. Works after
the sandbox is destroyed, until its events are pruned. The caller must own the
sandbox or hold `audit_read: "all"`; otherwise `404`.

## Token policy: `audit_read`

`"audit_read": "all"` in a token's `moatd.json` entry lets it read every
token's audit events and run `moat audit verify` over the API. Omitted or
empty means the token sees only its own.

## Observability

- `GET /metrics` — Prometheus text. Series include
  `moat_sandboxes_active{template}`,
  `moat_sandboxes_created_total{template,source}`,
  `moat_sandbox_create_seconds{source}`,
  `moat_exec_total{outcome}`, `moat_file_bytes_total{direction}`,
  `moat_secret_findings_total{rule,action}`,
  `moat_quota_rejects_total`, `moat_pool_ready`,
  `moat_http_requests_total{route,status}`,
  `moat_egress_requests_total{decision}`.
- Request logs are one structured JSON line per request (`method`, `route`,
  `status`, `dur_ms`).

## curl walk-through

```sh
T='Authorization: Bearer s3cr3t'
ID=$(curl -s -XPOST -H "$T" -d '{"ttl_seconds":120}' localhost:8080/v1/sandboxes | jq -r .id)
curl -s -XPOST -H "$T" -d '{"argv":["uname","-a"]}' localhost:8080/v1/sandboxes/$ID/exec
curl -s -XPUT  -H "$T" --data-binary @./script.py localhost:8080/v1/sandboxes/$ID/files/tmp/s.py
curl -s -XPOST -H "$T" -d '{"argv":["python3","/tmp/s.py"]}' localhost:8080/v1/sandboxes/$ID/exec
curl -s -XDELETE -H "$T" localhost:8080/v1/sandboxes/$ID
```
