# Runner image

incuse VMs run Ubuntu 24.04 with the actions/runner installed at boot via cloud-init. No image baking, no snapshot lifecycle — just `incus launch` against the upstream cloud image and a per-launch `#cloud-config` payload.

## Base image

`images:ubuntu/24.04/cloud` from `https://images.linuxcontainers.org` (simplestreams).

Why this image:
- Ships `incus-agent` (so `incus exec` and config injection work out of the box).
- Ships `cloud-init` (so the per-launch user-data runs at first boot).
- Closest match to the GitHub-hosted `ubuntu-latest` runner — minimises surprises for jobs that worked there.

amd64 only for the MVP — that's the hardware in production.

## Runner version

incuse always tracks the latest published GitHub Actions runner. At startup (and on a 1-hour ticker) the orchestrator hits `GET https://api.github.com/repos/actions/runner/releases/latest`, picks the linux-x64 asset, and stuffs the resulting download URL into the cloud-init template.

Trade-offs:
- ✅ Zero per-launch API calls — a 100-runner burst hits the GitHub API once.
- ✅ Always-current.
- ❌ Sha256 not pinned (the runner repo doesn't publish per-asset checksums in the API). Trust currently comes from HTTPS to github.com. Tightening this is a follow-up if supply-chain hardening becomes a priority.

## What cloud-init does

In order:
1. Sets the hostname to the minted runner name (so `incus list` and runner names match).
2. Creates a `runner` user in the `sudo` and `docker` groups, with passwordless sudo (matches actions/runner expectations).
3. Installs `curl`, `tar`, `git`, `jq`, `ca-certificates`, `docker.io`.
4. Writes `/etc/incuse/jit.env` (mode 0600, owned by runner) with the JIT config blob.
5. Drops a systemd unit `incuse-runner.service` (`Type=oneshot`, `EnvironmentFile=/etc/incuse/jit.env`, `ExecStart=/opt/runner/run.sh --jitconfig ${INCUSE_JIT}`, `ExecStopPost=/sbin/poweroff`).
6. Downloads the runner tarball, extracts to `/opt/runner`, fixes ownership.
7. Starts docker.
8. Enables and starts `incuse-runner.service`.

When the runner finishes its job (or is cancelled), `run.sh` exits, systemd fires the post-stop hook, the VM powers off, the orchestrator sees the stopped state and deletes the instance.

## Why VMs (and not system containers)

Docker is mandatory: every non-trivial Actions workflow uses docker actions or `services:`. Running Docker inside an Incus system container needs `security.nesting=true` and a careful storage-driver dance, and you still hit edge cases (cgroup v2 quirks, AppArmor confusion, `--privileged` interactions). VMs sidestep all of that — Docker inside the VM is just Docker on Linux.

Container support is parked as a follow-up: cheap to revisit if a workload that doesn't need docker would benefit from container-speed boots.

## Incus profile (`incuse-runner`)

Recommended profile for the `incuse` project. Concrete values vary with vCPU tier and host bridge — capture rocket-specific bits in `docs/deployment.md`.

```
config:
  limits.cpu: "2"
  limits.memory: 8GiB
  security.secureboot: "false"     # speeds boot; not required
devices:
  root:
    type: disk
    pool: default
    path: /
    size: 40GiB
  eth0:
    type: nic
    nictype: bridged
    parent: <host-bridge>
```

VM-only — no `security.nesting`, no `security.privileged`. All isolation comes from the hypervisor.

## Pre-baking (follow-up)

If cold-boot + cloud-init + runner-download consistently exceeds the registration timeout (`runner.registration_timeout`, default 10 min), we'll bake an image with the runner pre-installed. Two options on the table:

- **Packer-against-Incus**: standard pipeline, but Packer's Incus builder is third-party.
- **Periodic build VM**: a daily systemd timer launches a build VM that runs the cloud-init, then publishes a snapshot back as an Incus image alias (`incuse/runner:latest`). incuse then `incus launch incuse/runner:latest` instead of the upstream cloud image.

Decide after phase-4 smoke timings.
