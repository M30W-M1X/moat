# Template skeleton

This directory is a starting point for a new microVM rootfs template. It is
not built on its own.

## Make a template

1. Copy this directory:

   ```sh
   make template NAME=python      # copies _skeleton to build/templates/python
   ```

2. Edit `build/templates/python/Dockerfile`. Add the packages your workload
   needs to the `RUN apk add` line. Keep the `FROM` digest, or set a current
   one.

3. Edit `build/templates/python/template.env`. Raise `ROOTFS_SIZE` if the
   userland is large.

4. Build the image:

   ```sh
   make template-image NAME=python   # writes build/artifacts/python-rootfs.ext4
   ```

5. Point `moatd` at it. Add a template entry to `moatd.json`:

   ```json
   { "name": "python", "kernel": "/var/lib/moat/vmlinux",
     "rootfs": "/var/lib/moat/python-rootfs.ext4", "vcpus": 2, "mem_mib": 512 }
   ```

## Rules

- **Pin the base image by digest.** A tag moves. A digest does not.
- **No `ENTRYPOINT` or `CMD`.** The build installs `/sbin/guest-agent` and
  the kernel runs it as PID 1.
- The build records the installed packages in `packages.manifest`. Commit
  that file so a reviewer can see what changed.
- Templates are not pinned in `images.lock`. Two builds on one day match.
  Builds weeks apart can differ, because Alpine package versions move.

See `build/templates/README.md` for the full picture.
