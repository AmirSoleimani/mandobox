# Golden guest image (M3)

Content-addressed `rootfs-<sha>.ext4.zst` the guest boots from (PLAN §7.2). `fc-supervisor`
is PID 1 — no systemd, sshd, cron, dbus, or cloud-init.

## Contents

Minimal Debian bookworm + Node LTS, **pinned** Claude Code, `git`, `gh`, `ripgrep`, `fd`,
`jq`, and language runtimes/linters (Go + golangci-lint, Python + ruff). Per **D1** the guest
verifies with linters + language unit tests; the full docker-compose/e2e suite runs on the
target repo's own **GitHub CI** (§6.1 `ci_status`, §11 `check_suite`) — so no Docker here.

Pinned versions live as `ARG`s in `Dockerfile` and are recorded in `/etc/fleet-image-versions`
inside the image. Bump them there.

## Build

Runs on Linux with Docker + `e2fsprogs` + `zstd` (CI does this automatically):

```sh
bash image/build.sh          # -> dist/images/rootfs-<sha>.ext4.zst  (prints the sha)
SIZE_MB=2048 bash image/build.sh
```

`build.sh` does: `docker build` → `docker export` the rootfs → `mke2fs -d` (populates ext4
with no privileged loop mount) → `zstd`. The sha is the sha256 of the **uncompressed** ext4.

CI: `.github/workflows/golden-image.yml` builds on changes to `image/`, the supervisor, or
`go.{mod,sum}` and uploads the artifact. Uncomment the object-storage step to publish to R2/S3.

## How the image reaches a VM

1. CI publishes `rootfs-<sha>.ext4.zst` to object storage.
2. The fleet host caches it under `/var/lib/fleet/images/`.
3. The workflow pins `image_sha` per PR and passes it to `fleet-agent`, which
   `EnsureRootfs` (decompress) → reflink-copies it per launch (§7.1). Pinning the sha per
   workflow keeps a PR on one image across resume rounds (§7.2).

The guest **kernel** is the pinned M1 CI kernel (`vmlinux-6.1.155`) — dropping Docker means
no custom kernel is needed.
