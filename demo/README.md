# Moat demo

`run.sh` is a scripted, self-checking walk-through of Moat. Use it as proof
the system works and as a live demo.

## Run it

```sh
make demo-image        # once: build the dev-shell rootfs (~600 MiB, a few minutes)
make kernel            # once: fetch the pinned vmlinux
./demo/run.sh
```

The script needs `/dev/kvm`, `curl`, and `jq`. It builds `moatd` and `moat`
itself. It exits non-zero on the first failed check.

## What it shows

| Step | Property | Check |
| --- | --- | --- |
| Start the control plane | `moatd` serves the HTTP API | `/healthz` answers |
| Create a sandbox | one microVM per `POST /v1/sandboxes` | `moat_sandboxes_active` = 1 |
| Prove the microVM boundary | the guest runs its own kernel, PID 1 is the agent | guest `uname -r` is `6.1.x`, `/proc/1/comm` is `guest-agent` |
| Run a real build | put files, `go test`, `go build` in the sandbox | test exit 0, binary runs |
| Copy the binary out | `GET .../files/...` streams the artifact back | the file is an ELF executable |
| Ingress secret firewall | a credential in an upload is blocked | `PUT` returns `409`, the file is absent in the guest |
| One microVM per task | each sandbox has a private rootfs | sandbox B cannot read sandbox A's file |
| Sandboxes are ephemeral | a destroyed sandbox leaves nothing | a fresh sandbox has an empty `/work` |
| The template store | pick a userland per sandbox | a `rhel9` sandbox reports `Red Hat Enterprise Linux release 9`, has `dnf` + `python3`; the default is Alpine/musl — same kernel, same API |
| Import a container as a template | `moat template import` builds a rootfs from an OCI image | the image `ENTRYPOINT`/`CMD` runs on an argv-less exec; `WORKDIR` and `ENV` apply; a request value wins |
| Network isolation and caps | netns + nftables + cgroups | reported; run only as root |
| The audit trail | every crossing is one hash-chained record | `moat audit verify` passes |

Network isolation and resource caps need a real root host. The script
reports what they do and skips them when it runs rootless.

## The sample workload

`demo/task/` is a tiny Go module (`wordcount`) with a test. The script
copies it into a sandbox, runs the test, builds the binary, and copies the
binary out. Swap in your own project to demo a different workload.

## Under the hood

At startup the script builds the `dev-shell` and `rhel9` template images,
runs `moat template import` to append a `demo-import` template to a
generated `moatd.json`, and starts `moatd -config` with all three. The
dev-shell steps use the default template; "The template store" step runs a
`rhel9` sandbox; the last template step uses `demo-import`. The `rhel9`
step skips itself when the Red Hat registry is unreachable.

The script uses only `curl` against the documented API (`docs/api.md`), plus
`moat template import` / `moat template show`. Read it top to bottom for a
worked example of every route.
