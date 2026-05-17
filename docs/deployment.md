# Deployment

This is the from-zero install path on a single Debian/Ubuntu host running Incus. The MVP target is `rocket.lkv.netwerk.io` (Debian 13, kernel 6.12.x, Incus 6.20+).

## 1. Prerequisites

Incus is installed and running, and you can reach the daemon as root:

```sh
incus version
incus list
```

If `getent group incus-admin` returns nothing, your Incus install is too old or non-standard — every recent build creates that group at install time.

## 2. Pre-create the Incus project + profile

The orchestrator launches into a dedicated project so it never collides with hand-managed instances.

```sh
incus project create incuse \
  -c features.images=true \
  -c features.profiles=true \
  -c features.storage.volumes=true

# Switch into the project for the next commands.
incus project switch incuse

incus profile create incuse-runner
incus profile edit incuse-runner <<'PROFILE'
description: incuse runner VM profile
config:
  security.secureboot: "false"
devices:
  root:
    type: disk
    path: /
    pool: default
    size: 40GiB
  eth0:
    type: nic
    nictype: bridged
    parent: incusbr0
PROFILE

# Confirm.
incus profile show incuse-runner
```

Per-launch the orchestrator overrides `limits.cpu`, `limits.memory`, and the root disk size — the profile only carries the shape that's constant across runners.

## 3. Install incuse

Either install the release artefacts:

```sh
TAG=v0.1.0
mkdir -p /tmp/incuse-install && cd /tmp/incuse-install
curl -fsSLO "https://github.com/netwerk-io/incuse/releases/download/${TAG}/incuse-${TAG}-linux-amd64"
curl -fsSLO "https://github.com/netwerk-io/incuse/releases/download/${TAG}/incuse.service"
curl -fsSLO "https://github.com/netwerk-io/incuse/releases/download/${TAG}/incuse.example.yaml"
curl -fsSLO "https://github.com/netwerk-io/incuse/releases/download/${TAG}/install.sh"
curl -fsSLO "https://github.com/netwerk-io/incuse/releases/download/${TAG}/SHA256SUMS"
sha256sum -c SHA256SUMS
sudo bash install.sh "./incuse-${TAG}-linux-amd64"
```

Or, from a checkout (dev path):

```sh
make install-remote HOST=rocket.lkv.netwerk.io
```

The installer:

- creates the `incuse` system user with `incus-admin` as a supplementary group,
- drops the binary at `/usr/local/bin/incuse`,
- drops `incuse.example.yaml` at `/etc/incuse/config.yaml` (only if absent),
- creates `/var/lib/incuse` and `/etc/incuse` with the right ownership,
- installs `/etc/systemd/system/incuse.service` and runs `systemctl daemon-reload`.

It does **not** start the service — you still need to drop a credential and edit the config.

## 4. Drop a GitHub credential

PAT (MVP path):

```sh
# Generate a fine-grained PAT scoped to the org with
# `Self-hosted runners: Read & write`.
sudo install -m 0600 -o incuse -g incuse /dev/stdin /etc/incuse/github.pat <<<"ghp_..."
```

GitHub App (alternative):

```sh
sudo install -m 0600 -o incuse -g incuse <(cat private-key.pem) /etc/incuse/github-app.pem
# In config.yaml: set auth.mode: app and fill in the app block.
```

## 5. Edit `/etc/incuse/config.yaml`

At minimum:

- `github.config_url` — your org URL.
- `scale_set.name` — a name unique within the org.
- `scale_set.max_runners` — start small (e.g. `4`) and tune up.
- `runner.runner_version` + `runner.runner_sha256` — bump in lock-step. See `docs/runner-image.md` for how to fetch the current values.

Validate before starting:

```sh
sudo -u incuse /usr/local/bin/incuse --validate --config /etc/incuse/config.yaml
```

The preflight checks the config schema, every referenced credential / cert exists, and that secret files are at most `0600`. The systemd unit re-runs the same check via `ExecStartPre=`.

## 6. Start the service

```sh
sudo systemctl enable --now incuse
journalctl -u incuse -f
```

Within ~10 s you should see lines like:

```
{"msg":"orchestrator running","reap_interval":"30s","max_runners":4,"project":"incuse"}
```

Then trigger a workflow on a repo in your org with `runs-on: [incuse-rocket]` (or whatever you set `scale_set.name` to) and watch:

```sh
journalctl -u incuse -f
incus list --project incuse
```

A new VM should appear within seconds, the runner registers ~60 s later, the job runs, and the VM disappears via the cloud-init `poweroff` + Incus `Ephemeral=true` cleanup.

## Notes

- **HTTPS to a remote Incus daemon**: see `docs/incus-access.md` for the cert dance. The orchestrator picks transport by config presence: `incus.url` set → HTTPS, otherwise Unix socket.
- **systemd LoadCredential**: the unit ships with `LoadCredential=` commented out. Uncomment it if you'd rather have systemd manage the secret material under `/run/credentials/incuse.service/`. If you do, point `pat_file` (or `private_key_file`) at the literal `/run/credentials/incuse.service/<name>` path — the orchestrator does not currently expand environment variables in config paths.
- **First-boot debugging**: if the service flaps, `systemctl status incuse` shows the preflight error verbatim. Most failures are missing-file or wrong-mode on a credential.
