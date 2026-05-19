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

Default is VM. Docker is mandatory for many Actions workflows: every non-trivial workflow uses docker actions or `services:`. Running Docker inside an Incus system container needs `security.nesting=true` and a careful storage-driver dance, and you still hit edge cases (cgroup v2 quirks, AppArmor confusion, `--privileged` interactions). VMs sidestep all of that — Docker inside the VM is just Docker on Linux.

For workloads that **don't** need docker, system containers are a much faster alternative — cold-boot is ~2s vs ~30s for a VM, and there's no per-launch hypervisor overhead.

## System container mode

Set `runner.instance_type: container` in the config. Each launched instance becomes an Incus system container instead of a VM, with these security flags applied per-launch:

- `security.nesting: true` — nested user namespaces, required for most container runtimes / unshare workloads.
- `security.syscalls.intercept.mknod: true` and `security.syscalls.intercept.setxattr: true` — the standard "docker-in-LXC" pair (still wired even in non-privileged mode so kernel-syscall workloads work).

The rendered cloud-init drops the docker.io install and the `docker.service` systemd dependency. The runner user no longer joins the docker group. Jobs that try to run `docker` will fail — use VM mode for those, or set `runner.privileged: true` (below).

### Insecure mode (`privileged: true`)

Setting `runner.privileged: true` (only valid alongside `instance_type: container`) adds `security.privileged=true` to the launched container. The container runs as host root and a compromised job can affect the host kernel. Use only for trusted internal workloads where the speed of container start outweighs the loss of hypervisor isolation. Docker can run in this mode.

### Running both shapes on the same host

incuse is single-scale-set per process. To offer both VM and container runners on the same host, run two systemd units pointing at two configs:

```
/etc/systemd/system/incuse-vm.service        → /etc/incuse/vm.yaml      (instance_type: vm,        scale_set.name: incuse-rocket-vm)
/etc/systemd/system/incuse-container.service → /etc/incuse/container.yaml (instance_type: container, scale_set.name: incuse-rocket-container)
```

Workflows pick by label:

```yaml
# fast, no docker
runs-on: [self-hosted, incuse-rocket-container]

# docker-needing
runs-on: [self-hosted, incuse-rocket-vm]
```

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

VM-only by default — no `security.nesting`, no `security.privileged`. All isolation comes from the hypervisor.

For `instance_type: container` use a separate profile (e.g. `incuse-runner-container`). The `security.secureboot` key is VM-only and must not be set on container profiles. The minimum viable container profile:

```
config:
  limits.cpu: "2"
  limits.memory: 8GiB
devices:
  root:
    type: disk
    pool: default
    path: /
    size: 40GiB
  eth0:
    type: nic
    nictype: macvlan
    parent: <host-interface>
```

incuse adds the per-launch `security.nesting`, `security.syscalls.intercept.*`, and (when `privileged: true`) `security.privileged` config keys on top of whatever the profile defines.

## Pre-baked image (`use_baked_image: true`)

Vanilla flow runs `apt-get install` + downloads actions/runner on every cold boot, costing ~60-70s per VM. The baked-image flow does that work once, then each spawned VM only pays for kernel boot + cloud-init drop-of-jit + service start (~25-35s on a 1-vCPU VM).

### Build the image

On the Incus host, as a user that can talk to the daemon (root or in `incus-admin`):

```bash
RUNNER_VERSION=2.334.0 \
  RUNNER_SHA256=048024cd2c848eb6f14d5646d56c13a4def2ae7ee3ad12122bee960c56f3d271 \
  bash scripts/build-runner-image.sh
```

What it does, briefly: launches `images:ubuntu/24.04/cloud` as `incuse-builder`, `apt-get install`s the runtime deps, creates the `runner` user with `NOPASSWD` sudo + docker group, downloads + sha-checks + extracts actions/runner into `/opt/runner`, populates `/opt/hostedtoolcache` with Node / Python / Go (last 3 majors of each) plus `gh`, `aws`, `az` system-wide, drops `/etc/systemd/system/incuse-runner.service`, runs `cloud-init clean`, stops the VM, `incus publish`s as `incuse-runner-v<ver>`, points the floating `incuse-runner` alias at the new fingerprint.

### Building a container image

Same script, set `INSTANCE_TYPE=container`. Defaults swap accordingly:

| | VM (default) | Container |
|---|---|---|
| `incus launch` flag | `--vm` | (none) |
| Default profile | `incuse-runner` | `incuse-runner-container` |
| Image alias | `incuse-runner` | `incuse-runner-container` |
| docker.io install | yes | no (set `WITH_DOCKER=1` to override) |

```bash
INSTANCE_TYPE=container \
  RUNNER_VERSION=2.334.0 \
  RUNNER_SHA256=048024cd2c848eb6f14d5646d56c13a4def2ae7ee3ad12122bee960c56f3d271 \
  bash scripts/build-runner-image.sh
```

The container profile (`incuse-runner-container` by default) must exist before running the script — see the profile section below.

### Refreshing the toolcache

The script bakes specific patch versions of Node / Python / Go. Override the defaults to pin or refresh:

```bash
TOOLCACHE_NODE_VERSIONS="22.11.0 24.0.0" \
  TOOLCACHE_PYTHON_VERSIONS="3.12.7 3.13.0" \
  TOOLCACHE_GO_VERSIONS="1.24.4 1.25.3" \
  RUNNER_VERSION=... RUNNER_SHA256=... \
  bash scripts/build-runner-image.sh
```

Layout matches what `actions/setup-{node,python,go}` expects:

```
/opt/hostedtoolcache/node/<ver>/x64/
/opt/hostedtoolcache/Python/<ver>/x64/
/opt/hostedtoolcache/go/<ver>/x64/
```

With a `<ver>/x64.complete` sentinel file alongside each tree so the setup actions skip download. The runner unit sets `Environment=AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache` so the actions find it.

Re-run with a new `RUNNER_VERSION` whenever actions/runner releases a new version. The `--reuse` flag on `incus publish` makes re-runs idempotent.

### Switch incuse to the baked image

In `/etc/incuse/config.yaml`:

```yaml
runner:
  image_alias: incuse-runner       # the floating alias from the build script
  use_baked_image: true            # tells incuse to use the minimal cloud-init template
  runner_version: 2.334.0          # informational; baked into the image
  runner_sha256: 048024cd2c848eb6f14d5646d56c13a4def2ae7ee3ad12122bee960c56f3d271
```

Leave `image_server` / `image_protocol` unset (or empty). incuse looks up the alias on the local Incus daemon, not from a remote simplestreams server.

```bash
sudo -u incuse /usr/local/bin/incuse --validate --config /etc/incuse/config.yaml
sudo systemctl restart incuse
```

### When to refresh

- New actions/runner release (security or feature). Bump `RUNNER_VERSION` + `RUNNER_SHA256`, re-run.
- Critical Ubuntu base-image security update. Re-run with the same `RUNNER_VERSION`; the script will re-pull the latest `images:ubuntu/24.04/cloud` and re-install everything on top.

incuse picks up the new image on its next runner spawn. Already-running VMs are unaffected.

### Trade-offs

- **Pro**: ~60-70s faster pickup. P50 drops from ~95s to ~25-35s on a 1-vCPU VM.
- **Con**: image is now your responsibility — stale base-image security updates land later.
- **Con**: harder to debug "vanilla works but baked doesn't" — try `use_baked_image: false` to force the heavyweight path if a job is failing weirdly.

