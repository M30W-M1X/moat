---
name: moat
description: Run untrusted, generated, or destructive commands inside a disposable hardware-isolated microVM instead of on the user's machine. Use for package installs, running generated code, network fetches, and large-scale file changes.
---

# Moat — disposable sandboxes for commands you should not run on the host

Moat gives you a throwaway Firecracker microVM: its own Linux kernel, no access
to the user's files, network, or credentials, destroyed after the task. Use it
whenever a command could damage the host or leak data.

The tools come from the `moat` MCP server: `moat_create`, `moat_write_file`,
`moat_exec`, `moat_read_file`, `moat_list`, `moat_destroy`.

## When to use a sandbox

Put a command in Moat when any of these are true:

- It installs software (`pip install`, `npm install`, `apt`, `curl … | sh`).
- It runs code you or the model generated and have not fully read.
- It fetches from the network.
- It changes many files, or deletes files.
- You would not run it on the user's machine without review.

Run trivial, read-only commands (`ls`, `cat`, `git status`) directly. A
sandbox is cheap but not free.

## The loop

1. **`moat_create`** — get a sandbox `id`. Pass `network: "deny"` unless the
   task needs the internet; then use `network: "egress"` (traffic still goes
   through a domain filter). Set `ttl_seconds` if the task is long.
2. **`moat_write_file`** — put in the code and data the task needs. Paths are
   absolute in the guest, e.g. `/work/main.py`.
3. **`moat_exec`** — run the command. You get `exit`, `stdout`, `stderr`, and
   `timed_out`. Output is captured, not streamed, and is truncated if huge.
   Repeat as needed.
4. **`moat_read_file`** — pull out results.
5. **`moat_destroy`** — always, when the task is done. The VM and its disk are
   discarded. TTL and idle timeout also reap it, but do not rely on that.

Each task starts from a clean sandbox. State does not carry over.

## Reading results

- `exit: 0` means success. Non-zero is the command's own exit code.
- `timed_out: true` means Moat killed the command at the timeout.
- `signal` is set when a signal killed the command.
- `truncated: true` means the output was longer than the cap; re-run writing
  output to a file and use `moat_read_file`.

## When the secret firewall blocks you

`moat_write_file` and `moat_exec` scan everything you send in. A block comes
back as an **error result** naming the rule and line, for example:

> BLOCKED by the secret firewall: /work/.env — aws-secret-key at line 3

When this happens:

- **Do not** retry with `allow_secrets: true` on your own.
- Tell the user what was found and where.
- Only set `allow_secrets: true` if the user explicitly tells you to send that
  specific content.
- Never put the user's real credentials into a sandbox. If the task needs a
  credential, use a dummy value, or ask the user how they want to handle it.

## Audit log

Every action you take in a sandbox — the commands you run, the files you move,
a blocked secret, a denied network destination — is recorded in the host's
audit log. This does not change how you work; it means the user can review
exactly what a sandbox did.

## Defaults to prefer

- `network: "deny"` unless a fetch is required.
- One sandbox per task; destroy it when done.
- Write generated code to a file and run the file, rather than a long
  `-c "..."` one-liner, so errors point at line numbers.
