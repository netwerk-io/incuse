# Operations

How to run incuse day-to-day on the host.

## Logs

incuse logs JSON to stderr; systemd captures it.

```sh
# Live tail.
journalctl -u incuse -f

# Last 200 lines, pretty-printed.
journalctl -u incuse -n 200 -o cat | jq -c .

# Filter by runner name.
journalctl -u incuse -o cat | jq -c 'select(.runner_name == "incuse-rocket-aaaa")'
```

Useful structured fields incuse always sets when relevant: `runner_name`, `runner_request_id`, `scale_set_id`, `vcpu`, `mem_mb`, `disk_gb`, `arch`, `error`, `reason`.

## Inspecting in-flight runners

The orchestrator's view, indirectly via Incus:

```sh
incus list --project incuse
```

Anything tagged `user.incuse.managed=true` is ours:

```sh
incus list --project incuse \
  -c name,status,config:user.incuse.runner_name,config:user.incuse.job_id,config:user.incuse.minted_at \
  -f csv
```

If you see an instance whose `minted_at` is older than `runner.registration_timeout` (default 10m) and the orchestrator hasn't reaped it, either the reaper is not running (check `journalctl -u incuse`) or the orchestrator process has lost track of it (drift sweep should catch it on the next 30s tick).

## Reaping orphans manually

If incuse is down and you want to clean up before restart:

```sh
incus list --project incuse -c name,config:user.incuse.managed -f csv \
  | awk -F, '$2 == "true" {print $1}' \
  | xargs -r -I{} incus delete --force --project incuse {}
```

Once incuse is back up, the drift sweep picks up anything you missed within 30s.

## Reading the scale set on GitHub

Every runner registration shows up under `Settings → Actions → Runners → <scale set name>` on the org. Idle entries with no matching VM in `incus list` are GitHub-side stragglers — they expire on their own (default ~14 days), but `gh api` works to clear them out:

```sh
gh api -X GET "/orgs/vegardx/actions/runners?per_page=100" \
  | jq -r '.runners[] | select(.status=="offline") | .id' \
  | xargs -r -I{} gh api -X DELETE "/orgs/vegardx/actions/runners/{}"
```

## Rotating the GitHub PAT

```sh
sudo install -m 0600 -o incuse -g incuse /dev/stdin /etc/incuse/github.pat <<<"ghp_new..."
sudo systemctl restart incuse
```

Validate before restart if you'd rather catch a bad token without dropping in-flight jobs:

```sh
sudo -u incuse /usr/local/bin/incuse --validate --config /etc/incuse/config.yaml
```

## Changing config

The orchestrator currently reloads on restart only — there is no SIGHUP handler. For changes:

```sh
sudo systemctl restart incuse
```

In-flight runners (Incus VMs that have already started) are unaffected; the reaper picks them up on the next sweep after restart via the drift-sweep path.

## Draining

`systemctl stop incuse` cancels the orchestrator's context, which:

- stops the listener (no new JobAssigned → no new launches),
- returns the reaper goroutine,
- closes the scaleset session.

In-flight VMs keep running their jobs and self-poweroff via the cloud-init `ExecStopPost=/sbin/poweroff` path. Their Incus instance entries linger until the next time incuse runs (drift sweep). This is fine; it just means restart picks them up.

If you need a faster drain:

```sh
incus list --project incuse -c name,config:user.incuse.managed -f csv \
  | awk -F, '$2 == "true" {print $1}' \
  | xargs -r -I{} incus delete --force --project incuse {}
```

## Upgrades

```sh
TAG=v0.2.0
cd /tmp/incuse-install
curl -fsSLO "https://github.com/vegardx/incuse/releases/download/${TAG}/incuse-${TAG}-linux-amd64"
curl -fsSLO "https://github.com/vegardx/incuse/releases/download/${TAG}/SHA256SUMS"
sha256sum -c SHA256SUMS
sudo install -m 0755 "incuse-${TAG}-linux-amd64" /usr/local/bin/incuse
sudo systemctl restart incuse
```

The on-disk config and unit are unchanged across patch releases. Check the release notes for any minor-bump migration steps before upgrading.

## Health checks

Pre phase 7 (Prometheus + `/healthz`), the cheap shell-level check is:

```sh
systemctl is-active incuse
incus list --project incuse >/dev/null
```

Both green → orchestrator is up, the daemon socket works, and we have read access to our project.
