#!/usr/bin/env bash
# build-runner-image.sh — build a pre-baked incuse-runner Incus image.
#
# Bakes packages, the actions/runner tarball, the runner user, and
# the incuse-runner.service unit into a published Incus image so each
# spawned VM only pays for kernel boot + cloud-init drop-of-jit.env +
# starting the unit. Cuts pickup latency by ~60-70s on a 1-vCPU VM.
#
# Operator runs this once per actions/runner version bump on the
# Incus host (no remote build path):
#
#   sudo -u incuse RUNNER_VERSION=2.334.0 \
#     RUNNER_SHA256=048024cd2c848eb6f14d5646d56c13a4def2ae7ee3ad12122bee960c56f3d271 \
#     bash scripts/build-runner-image.sh
#
# After it succeeds, set in /etc/incuse/config.yaml:
#
#   runner:
#     image_alias: incuse-runner
#     use_baked_image: true
#
# and `systemctl restart incuse`.

set -euo pipefail

RUNNER_VERSION="${RUNNER_VERSION:?set RUNNER_VERSION (e.g. 2.334.0)}"
RUNNER_SHA256="${RUNNER_SHA256:?set RUNNER_SHA256 for the linux-x64 tarball}"
INCUS_PROJECT="${INCUS_PROJECT:-incuse}"
BUILD_NAME="${BUILD_NAME:-incuse-builder}"
BASE_IMAGE="${BASE_IMAGE:-images:ubuntu/24.04/cloud}"
BUILD_PROFILE="${BUILD_PROFILE:-incuse-runner}"
IMAGE_ALIAS_VERSIONED="incuse-runner-v${RUNNER_VERSION}"
IMAGE_ALIAS_LATEST="incuse-runner"

echo "==> launching build VM ${BUILD_NAME} from ${BASE_IMAGE} (profile=${BUILD_PROFILE})"
incus launch "$BASE_IMAGE" "$BUILD_NAME" --vm --project "$INCUS_PROJECT" --profile "$BUILD_PROFILE"

echo "==> waiting for incus-agent"
for _ in $(seq 1 90); do
	if incus exec --project "$INCUS_PROJECT" "$BUILD_NAME" -- true 2>/dev/null; then
		break
	fi
	sleep 1
done

echo "==> waiting for cloud-init to finish"
incus exec --project "$INCUS_PROJECT" "$BUILD_NAME" -- cloud-init status --wait

echo "==> installing packages and actions/runner v${RUNNER_VERSION}"
incus exec --project "$INCUS_PROJECT" "$BUILD_NAME" --env DEBIAN_FRONTEND=noninteractive -- bash -se <<EOF
set -euo pipefail

apt-get update
apt-get install -y curl tar git jq ca-certificates docker.io
apt-get autoremove -y
apt-get clean

if ! id runner >/dev/null 2>&1; then
	useradd --create-home --shell /bin/bash --groups sudo,docker runner
fi
echo 'runner ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/runner
chmod 0440 /etc/sudoers.d/runner

install -d -o runner -g runner -m 0755 /opt/runner /opt/runner/_work
cd /tmp
curl -fsSL "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-x64-${RUNNER_VERSION}.tar.gz" -o runner.tgz
echo "${RUNNER_SHA256}  runner.tgz" | sha256sum -c -
tar -xzf runner.tgz -C /opt/runner
chown -R runner:runner /opt/runner
rm -f runner.tgz

install -d -m 0750 -o root -g root /etc/incuse

# systemd-resolved on Ubuntu 24.04 sends parallel A/AAAA queries. Many
# home-LAN DNS servers don't answer AAAA at all, leaving the resolver
# to wait out a 20s timeout per AAAA query before falling back to the
# A response. With 10 simultaneous VMs that compounds to ~90s of
# DNS-stall before actions/runner can even start its handshake. Force
# upstream to public DNS that handles AAAA correctly.
mkdir -p /etc/systemd/resolved.conf.d
cat > /etc/systemd/resolved.conf.d/incuse.conf <<'RESOLVED'
[Resolve]
DNS=1.1.1.1 1.0.0.1
FallbackDNS=8.8.8.8 8.8.4.4
RESOLVED

cat > /etc/systemd/system/incuse-runner.service <<'UNIT'
[Unit]
Description=GitHub Actions runner (one-shot)
After=network-online.target docker.service
Wants=network-online.target docker.service

[Service]
Type=oneshot
User=runner
Group=runner
WorkingDirectory=/opt/runner
EnvironmentFile=/etc/incuse/jit.env
ExecStart=/opt/runner/run.sh --jitconfig \${INCUSE_JIT}
ExecStopPost=+/bin/sleep 15
ExecStopPost=+/sbin/poweroff
RemainAfterExit=no
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable docker.service

# Clear per-instance identity files. Without this, every VM cloned
# from the published image inherits the build VM's /etc/machine-id.
# systemd-networkd derives its DHCP DUID from machine-id, so a shared
# machine-id means every clone presents the same DHCP client
# identifier and the LAN's DHCP server hands them all the same IPv4
# lease — verified on rocket where 10 baked VMs with unique MACs all
# ended up with 192.168.1.169.
: > /etc/machine-id
rm -f /var/lib/dbus/machine-id
ln -sf /etc/machine-id /var/lib/dbus/machine-id
rm -f /var/lib/systemd/random-seed
# Stale DHCP leases / systemd-networkd state can also reuse the
# build VM's identity. Drop them.
rm -rf /var/lib/systemd/network/*
rm -rf /var/lib/dhcp/*
rm -rf /var/lib/dhcpcd/*
# SSH host keys regenerate on first boot via ssh-keygen.service when
# the files are absent; otherwise every clone has the same fingerprint.
rm -f /etc/ssh/ssh_host_*

# Reset cloud-init state so a fresh boot from this image picks up
# the per-launch user-data.
cloud-init clean --logs
EOF

echo "==> stopping build VM"
incus stop --project "$INCUS_PROJECT" "$BUILD_NAME"

echo "==> publishing as ${IMAGE_ALIAS_VERSIONED}"
# --reuse: re-running with the same RUNNER_VERSION refreshes the image.
incus publish --project "$INCUS_PROJECT" "$BUILD_NAME" \
	--alias "$IMAGE_ALIAS_VERSIONED" \
	--reuse

echo "==> repointing ${IMAGE_ALIAS_LATEST} alias"
incus image alias delete "$IMAGE_ALIAS_LATEST" --project "$INCUS_PROJECT" 2>/dev/null || true
FINGERPRINT=$(
	incus image list --project "$INCUS_PROJECT" --format json \
		| python3 -c "
import json, sys
imgs = json.load(sys.stdin)
for i in imgs:
    if any(a.get('name') == '$IMAGE_ALIAS_VERSIONED' for a in i.get('aliases') or []):
        print(i['fingerprint'])
        break
"
)
if [[ -z "$FINGERPRINT" ]]; then
	echo "could not find fingerprint for $IMAGE_ALIAS_VERSIONED" >&2
	exit 1
fi
incus image alias create --project "$INCUS_PROJECT" "$IMAGE_ALIAS_LATEST" "$FINGERPRINT"

echo "==> deleting build VM"
incus delete --project "$INCUS_PROJECT" "$BUILD_NAME"

echo "==> done"
incus image list --project "$INCUS_PROJECT"
