# microvmd HTTP API

`microvmd` is the northbound daemon. It serves a JSON API for sandbox lifecycle,
command execution, and file transfer.

## Run

```sh
MICROVMD_TOKENS="ci:s3cr3t,dev:hunter2" microvmd -addr :8080 -pool 4
```

Flags: `-addr`, `-config FILE`, `-tokens id:secret,...` (or `$MICROVMD_TOKENS`),
`-pool N`, `-global-max`, `-token-max`, `-default-ttl`, `-max-ttl`, `-kernel`,
`-rootfs`, `-vcpus`, `-mem`, `-cpu-max`, `-mem-max`, `-pids-max`, `-disk-bw`,
`-net-bw`, `-net-pool`, `-uplink`, `-egress-addr`, `-egress-allow`,
`-egress-deny`, `-egress-transparent`, `-json-log`.

## Config file

`-config FILE` reads a JSON file that defines the templates, the tokens, and a
per-token policy. It replaces `-tokens` and the single flag-built template. A
non-zero value in the file's `defaults` block overrides the matching flag. See
`docs/microvmd.example.json`.

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
`0` falls back to `token_max`; `max_ttl` empty falls back to `max_ttl`.

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
  "stdin": "<base64>", "tty": false, "timeout_ms": 0 }
```

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
`{ "bytes_written": N, "sha256": "…" }`.

### `GET /v1/sandboxes/{id}/files/{path}`

Stream `/{path}` back as `application/octet-stream`; the `X-Sha256` trailer holds
the digest. `404` when the file does not exist.

## Observability

- `GET /metrics` — Prometheus text. Series include
  `microvmd_sandboxes_active{template}`,
  `microvmd_sandboxes_created_total{template,source}`,
  `microvmd_sandbox_create_seconds{source}`,
  `microvmd_exec_total{outcome}`, `microvmd_file_bytes_total{direction}`,
  `microvmd_quota_rejects_total`, `microvmd_pool_ready`,
  `microvmd_http_requests_total{route,status}`,
  `microvmd_egress_requests_total{decision}`.
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
