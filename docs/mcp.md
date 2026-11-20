# moat-mcp — Model Context Protocol server

`moat-mcp` gives an AI agent tools to use Moat. It speaks MCP over stdio and
calls a running `moatd` over its HTTP API.

## Run it

Start `moatd` first, then point an MCP client at `moat-mcp`:

```json
{
  "mcpServers": {
    "moat": {
      "command": "moat-mcp",
      "env": {
        "MOAT_ADDR": "http://127.0.0.1:8080",
        "MOAT_TOKEN": "ci:s3cr3t"
      }
    }
  }
}
```

`-addr URL`, `-socket PATH`, and `-token id:secret` work instead of the
environment (`MOAT_ADDR`, `MOAT_SOCKET`, `MOAT_TOKEN`). `MOAT_ADDR` also
accepts `unix:///path/to/moatd.sock`.

## Tools

| Tool | Purpose |
| --- | --- |
| `moat_create` | Start a disposable sandbox. `network`: `deny` (default) or `egress`. |
| `moat_write_file` | Write a file into the guest. Screened for credentials. |
| `moat_exec` | Run one command, wait, return `exit` / `stdout` / `stderr` / `timed_out`. |
| `moat_read_file` | Read a file out of the guest. Binary comes back base64. |
| `moat_list` | List the token's sandboxes. |
| `moat_destroy` | Stop and discard a sandbox. |

A write or exec blocked by the secret firewall comes back as an error result
naming the rule and line. The agent playbook (`SKILL.md`) tells the agent to
surface that to the user rather than override it.

## Works with

Any MCP client — Claude Code, Claude Desktop, Cursor, Continue, and others.
