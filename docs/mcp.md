# moat-mcp — Hardware-isolated sandboxes for AI coding agents

`moat-mcp` connects any Model Context Protocol client — Claude Code, Claude Desktop, Cursor, Continue, Codex, or your own agent loop — to a local `moatd` daemon.

Instead of approving `pip install`, `npm install`, or generated scripts on your own machine, your agent runs them inside a disposable KVM microVM with no access to `~/.ssh`, `~/.aws`, your LAN, or your files — and destroys the VM when the task finishes.

## Configure your MCP client

### Option 1: Local Unix socket (recommended on the same machine)

No open TCP port. Uses the `0660` socket at `/run/moat/moatd.sock` plus a bearer token from `moatd.json`:

```json
{
  "mcpServers": {
    "moat": {
      "command": "moat-mcp",
      "args": ["--socket", "/run/moat/moatd.sock"],
      "env": {
        "MOAT_TOKEN": "dev:<secret>"
      }
    }
  }
}
```

### Option 2: HTTP or HTTPS (`MOAT_ADDR`)

```json
{
  "mcpServers": {
    "moat": {
      "command": "moat-mcp",
      "env": {
        "MOAT_ADDR": "http://127.0.0.1:8080",
        "MOAT_TOKEN": "dev:<secret>"
      }
    }
  }
}
```

Flags `-addr URL`, `-socket PATH`, and `-token id:secret` override `$MOAT_ADDR`, `$MOAT_SOCKET`, and `$MOAT_TOKEN`. `MOAT_ADDR` also accepts `unix:///run/moat/moatd.sock`.

## Add the agent playbook (`SKILL.md`)

Point your agent's system instructions or skills directory at [`SKILL.md`](../SKILL.md) (installed at `/var/lib/moat/SKILL.md` or `~/.local/share/moat/SKILL.md`). It teaches the model:

- Which commands belong in a sandbox (installs, generated code, network fetches, destructive edits) and which trivial read-only checks do not.
- The `create → write → exec → read → destroy` loop, defaulting to `network: "deny"`.
- Never to silently override a secret-firewall block or place real credentials inside a sandbox.

## Tools

Six verbs cover the entire sandbox lifecycle:

| Tool | Purpose |
| --- | --- |
| `moat_create` | Start a fresh, disposable microVM. `network`: `deny` (default) or `egress` (domain-filtered). Optional `template` and `ttl_seconds`. |
| `moat_write_file` | Copy text or base64 binary into the guest. Screened by the ingress secret firewall before a byte enters the VM. |
| `moat_exec` | Run one command with optional `env`, `dir`, `stdin`, and `timeout_sec`. Returns `exit`, `signal`, `timed_out`, `stdout`, and `stderr`. |
| `moat_read_file` | Pull a result file back out of the guest. Text returns as UTF-8; binary returns as base64. |
| `moat_list` | List active sandboxes owned by this token. |
| `moat_destroy` | Stop the VM and discard its disk immediately. |

## What a secret-firewall block looks like

If an agent tries to upload a `.env` file or pass a credential in `env`/`stdin`, `moatd` rejects the call with `409` before the bytes reach the microVM. `moat-mcp` returns a structured error to the model naming the exact rule and line:

```text
BLOCKED by the secret firewall: secret detected in /work/.env — not sent
  aws-secret-key at line 3 (aZ****(40 chars))
Do not retry with allow_secrets unless the user has explicitly told you to send this.
Never put the user's real credentials into a sandbox.
```

## Review what the agent did

Every sandbox creation, command, file transfer, secret block, and egress decision lands in the hash-chained audit log. Inspect any session — even after the VM is gone:

```sh
moat audit report sb-7f3a19c4
moat audit verify
```
