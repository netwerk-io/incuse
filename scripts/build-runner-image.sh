#!/usr/bin/env bash
# build-runner-image.sh — build a pre-baked incuse-runner Incus image.
#
# Bakes packages, the actions/runner tarball, the runner user, the
# incuse-runner.service unit, and the GitHub-Actions-compatible
# /opt/hostedtoolcache (Node, Python, Go, gh/aws/az) into a published
# Incus image so each spawned instance only pays for kernel boot +
# cloud-init drop-of-jit.env + starting the unit. Cuts pickup latency
# by ~60s on a VM, ~25s on a container, and avoids per-job
# `actions/setup-{node,python,go}` downloads.
#
# Operator runs this once per actions/runner version bump (or when
# the toolcache versions need refreshing) on the Incus host:
#
#   sudo -u incuse RUNNER_VERSION=2.334.0 \
#     RUNNER_SHA256=048024cd2c848eb6f14d5646d56c13a4def2ae7ee3ad12122bee960c56f3d271 \
#     bash scripts/build-runner-image.sh
#
# To build a container image instead of a VM image:
#
#   INSTANCE_TYPE=container BUILD_PROFILE=incuse-runner-container \
#     RUNNER_VERSION=... RUNNER_SHA256=... \
#     bash scripts/build-runner-image.sh
#
# After it succeeds, set in /etc/incuse/config.yaml:
#
#   runner:
#     image_alias: incuse-runner            # or incuse-runner-container
#     use_baked_image: true
#     instance_type: vm                     # or container
#
# and `systemctl restart incuse`.

set -euo pipefail

RUNNER_VERSION="${RUNNER_VERSION:?set RUNNER_VERSION (e.g. 2.334.0)}"
RUNNER_SHA256="${RUNNER_SHA256:?set RUNNER_SHA256 for the linux-x64 tarball}"
INCUS_PROJECT="${INCUS_PROJECT:-incuse}"
BUILD_NAME="${BUILD_NAME:-incuse-builder}"
BASE_IMAGE="${BASE_IMAGE:-images:ubuntu/24.04/cloud}"
INSTANCE_TYPE="${INSTANCE_TYPE:-vm}"

# Toolcache versions. Override to add/remove versions or pin patches.
# Default tracks "last 3 majors" of each tool. Versions can be either
# a major ("20", "3.12", "1.25") or a full patch ("20.18.1",
# "3.12.7", "1.25.3"). Major-only is auto-resolved to upstream-latest
# patch at build time — each image rebuild picks up newer patches.
TOOLCACHE_NODE_VERSIONS="${TOOLCACHE_NODE_VERSIONS:-20 22 24}"
TOOLCACHE_PYTHON_VERSIONS="${TOOLCACHE_PYTHON_VERSIONS:-3.11 3.12 3.13}"
TOOLCACHE_GO_VERSIONS="${TOOLCACHE_GO_VERSIONS:-1.23 1.24 1.25}"

case "$INSTANCE_TYPE" in
	vm)
		BUILD_PROFILE="${BUILD_PROFILE:-incuse-runner}"
		IMAGE_ALIAS_VERSIONED="${IMAGE_ALIAS_VERSIONED:-incuse-runner-v${RUNNER_VERSION}}"
		IMAGE_ALIAS_LATEST="${IMAGE_ALIAS_LATEST:-incuse-runner}"
		LAUNCH_FLAGS=(--vm)
		;;
	container)
		BUILD_PROFILE="${BUILD_PROFILE:-incuse-runner-container}"
		IMAGE_ALIAS_VERSIONED="${IMAGE_ALIAS_VERSIONED:-incuse-runner-container-v${RUNNER_VERSION}}"
		IMAGE_ALIAS_LATEST="${IMAGE_ALIAS_LATEST:-incuse-runner-container}"
		LAUNCH_FLAGS=()
		;;
	*)
		echo "INSTANCE_TYPE must be 'vm' or 'container' (got: $INSTANCE_TYPE)" >&2
		exit 2
		;;
esac

# Whether to install + enable docker inside the image. Defaults match
# the most common case for each instance type — VM runners need
# docker for most CI workloads; non-privileged container runners
# can't run dockerd. Override with WITH_DOCKER=1 for the privileged-
# container case where you do want docker.
case "$INSTANCE_TYPE" in
	vm)        WITH_DOCKER="${WITH_DOCKER:-1}" ;;
	container) WITH_DOCKER="${WITH_DOCKER:-0}" ;;
esac

echo "==> launching build instance ${BUILD_NAME} (type=${INSTANCE_TYPE}, profile=${BUILD_PROFILE})"
incus launch "$BASE_IMAGE" "$BUILD_NAME" \
	"${LAUNCH_FLAGS[@]}" \
	--project "$INCUS_PROJECT" \
	--profile "$BUILD_PROFILE"

echo "==> waiting for incus-agent / exec readiness"
for _ in $(seq 1 90); do
	if incus exec --project "$INCUS_PROJECT" "$BUILD_NAME" -- true 2>/dev/null; then
		break
	fi
	sleep 1
done

echo "==> waiting for cloud-init to finish"
incus exec --project "$INCUS_PROJECT" "$BUILD_NAME" -- cloud-init status --wait

echo "==> installing packages, actions/runner v${RUNNER_VERSION}, toolcache, CLIs"
incus exec --project "$INCUS_PROJECT" "$BUILD_NAME" \
	--env DEBIAN_FRONTEND=noninteractive \
	--env "WITH_DOCKER=$WITH_DOCKER" \
	--env "RUNNER_VERSION=$RUNNER_VERSION" \
	--env "RUNNER_SHA256=$RUNNER_SHA256" \
	--env "TOOLCACHE_NODE_VERSIONS=$TOOLCACHE_NODE_VERSIONS" \
	--env "TOOLCACHE_PYTHON_VERSIONS=$TOOLCACHE_PYTHON_VERSIONS" \
	--env "TOOLCACHE_GO_VERSIONS=$TOOLCACHE_GO_VERSIONS" \
	-- bash -se <<'EOF'
set -euo pipefail

apt-get update
apt-get install -y curl tar git jq ca-certificates xz-utils unzip lsb-release gnupg
if [[ "$WITH_DOCKER" = "1" ]]; then
	apt-get install -y docker.io
fi

# Runner user. Joins the docker group only if docker is installed —
# adding to a non-existent group fails. Containers without docker
# get a runner that's only in sudo.
runner_groups="sudo"
if [[ "$WITH_DOCKER" = "1" ]]; then
	runner_groups="sudo,docker"
fi
if ! id runner >/dev/null 2>&1; then
	useradd --create-home --shell /bin/bash --groups "$runner_groups" runner
fi
echo 'runner ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/runner
chmod 0440 /etc/sudoers.d/runner

# actions/runner tarball
install -d -o runner -g runner -m 0755 /opt/runner /opt/runner/_work
cd /tmp
curl -fsSL "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-x64-${RUNNER_VERSION}.tar.gz" -o runner.tgz
echo "${RUNNER_SHA256}  runner.tgz" | sha256sum -c -
tar -xzf runner.tgz -C /opt/runner
chown -R runner:runner /opt/runner
rm -f runner.tgz

install -d -m 0750 -o root -g root /etc/incuse

# /opt/hostedtoolcache layout matches what actions/setup-* expects:
#   /opt/hostedtoolcache/<Tool>/<version>/<arch>/    (tool tree)
#   /opt/hostedtoolcache/<Tool>/<version>/<arch>.complete   (marker)
# The `.complete` sentinel is what actions/tool-cache writes on
# successful install; without it the setup-* actions treat the
# directory as a half-finished install and re-download.
install -d -o runner -g runner -m 0755 /opt/hostedtoolcache

# Node — official tarballs from nodejs.org. Spec can be a major
# ("20") or a full patch ("20.18.1"). Major-only resolves to latest
# via the per-major SHASUMS256.txt manifest.
for spec in $TOOLCACHE_NODE_VERSIONS; do
	if [[ "$spec" =~ ^[0-9]+$ ]]; then
		ver=$(curl -fsSL "https://nodejs.org/dist/latest-v${spec}.x/SHASUMS256.txt" \
			| awk '/linux-x64\.tar\.xz$/ {print $2; exit}' \
			| sed -E 's/^node-v([0-9.]+)-.*/\1/')
	else
		ver="$spec"
	fi
	if [[ -z "$ver" ]]; then
		echo "could not resolve Node $spec" >&2; exit 1
	fi
	echo "  -> Node $ver"
	dir="/opt/hostedtoolcache/node/$ver/x64"
	install -d -o runner -g runner -m 0755 "$dir"
	curl -fsSL "https://nodejs.org/dist/v$ver/node-v$ver-linux-x64.tar.xz" \
		| tar -xJ --strip-components=1 -C "$dir"
	chown -R runner:runner "/opt/hostedtoolcache/node/$ver"
	touch "/opt/hostedtoolcache/node/$ver/x64.complete"
done

# Python — actions/python-versions prebuilt tarballs. Their release
# tags include a build-id suffix ("3.12.7-12345"), so we can't
# construct the URL from a plain version: query the GH API for the
# matching tag, then download the linux-24.04-x64 asset.
for spec in $TOOLCACHE_PYTHON_VERSIONS; do
	if [[ "$spec" =~ ^[0-9]+\.[0-9]+$ ]]; then
		prefix="$spec."
	else
		prefix="$spec-"
	fi
	tag=$(curl -fsSL 'https://api.github.com/repos/actions/python-versions/releases?per_page=100' \
		| jq -r --arg p "$prefix" '
			[.[] | .tag_name
			  | select(startswith($p))
			  | select(test("-rc|-alpha|-beta") | not)]
			| sort_by(split("-")[0] | split(".") | map(tonumber))
			| reverse | .[0] // empty')
	if [[ -z "$tag" ]]; then
		echo "could not resolve Python $spec from actions/python-versions" >&2; exit 1
	fi
	ver="${tag%%-*}"
	echo "  -> Python $ver (tag=$tag)"
	dir="/opt/hostedtoolcache/Python/$ver/x64"
	install -d -o runner -g runner -m 0755 "$dir"
	curl -fsSL "https://github.com/actions/python-versions/releases/download/$tag/python-$ver-linux-24.04-x64.tar.gz" \
		| tar -xz -C "$dir"
	if [[ -x "$dir/setup.sh" ]]; then
		(cd "$dir" && ./setup.sh)
	fi
	chown -R runner:runner "/opt/hostedtoolcache/Python/$ver"
	touch "/opt/hostedtoolcache/Python/$ver/x64.complete"
done

# Go — official tarballs from go.dev/dl. The /?mode=json endpoint
# lists current stable + previous-stable releases; we filter for the
# requested major and pick the highest patch.
GO_RELEASES=$(curl -fsSL 'https://go.dev/dl/?mode=json&include=all')
for spec in $TOOLCACHE_GO_VERSIONS; do
	if [[ "$spec" =~ ^[0-9]+\.[0-9]+$ ]]; then
		ver=$(echo "$GO_RELEASES" | jq -r --arg s "$spec" '
			[.[] | .version | sub("^go"; "")
			  | select(startswith($s + "."))
			  | select(test("rc|beta") | not)]
			| sort_by(split(".") | map(tonumber))
			| reverse | .[0] // empty')
	else
		ver="$spec"
	fi
	if [[ -z "$ver" ]]; then
		echo "could not resolve Go $spec" >&2; exit 1
	fi
	echo "  -> Go $ver"
	dir="/opt/hostedtoolcache/go/$ver/x64"
	install -d -o runner -g runner -m 0755 "$dir"
	curl -fsSL "https://go.dev/dl/go$ver.linux-amd64.tar.gz" \
		| tar -xz --strip-components=1 -C "$dir"
	chown -R runner:runner "/opt/hostedtoolcache/go/$ver"
	touch "/opt/hostedtoolcache/go/$ver/x64.complete"
done

# System CLIs — gh, aws, az — installed to /usr/local/bin so they're
# on PATH for any user without setup actions.
echo "  -> gh"
mkdir -p -m 755 /etc/apt/keyrings
curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
	| tee /etc/apt/keyrings/githubcli-archive-keyring.gpg >/dev/null
chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
	> /etc/apt/sources.list.d/github-cli.list
apt-get update
apt-get install -y gh

echo "  -> aws CLI v2"
curl -fsSL "https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip" -o /tmp/awscliv2.zip
unzip -q /tmp/awscliv2.zip -d /tmp
/tmp/aws/install
rm -rf /tmp/aws /tmp/awscliv2.zip

echo "  -> Azure CLI"
curl -fsSL https://aka.ms/InstallAzureCLIDeb | bash

apt-get autoremove -y
apt-get clean
rm -rf /var/lib/apt/lists/*

cat > /etc/systemd/system/incuse-runner.service <<UNIT
[Unit]
Description=GitHub Actions runner (one-shot)
After=network-online.target$( [[ "$WITH_DOCKER" = "1" ]] && echo " docker.service" )
Wants=network-online.target$( [[ "$WITH_DOCKER" = "1" ]] && echo " docker.service" )

[Service]
Type=oneshot
User=runner
Group=runner
WorkingDirectory=/opt/runner
EnvironmentFile=/etc/incuse/jit.env
Environment=AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache
ExecStart=/opt/runner/run.sh --jitconfig \${INCUSE_JIT}
ExecStopPost=+/bin/sleep 15
ExecStopPost=+/sbin/poweroff
RemainAfterExit=no
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
UNIT

# systemd-resolved on Ubuntu 24.04 sends parallel A/AAAA queries. Many
# home-LAN DNS servers don't answer AAAA at all, leaving the resolver
# to wait out a 20s timeout per AAAA query before falling back to the
# A response. With 10 simultaneous instances that compounds to ~90s
# of DNS-stall before actions/runner can even start its handshake.
# Force upstream to public DNS that handles AAAA correctly.
mkdir -p /etc/systemd/resolved.conf.d
cat > /etc/systemd/resolved.conf.d/incuse.conf <<'RESOLVED'
[Resolve]
DNS=1.1.1.1 1.0.0.1
FallbackDNS=8.8.8.8 8.8.4.4
RESOLVED

systemctl daemon-reload
if [[ "$WITH_DOCKER" = "1" ]]; then
	systemctl enable docker.service
fi

# Clear per-instance identity files. Without this, every instance
# cloned from the published image inherits the build instance's
# /etc/machine-id. systemd-networkd derives its DHCP DUID from
# machine-id, so a shared machine-id means every clone presents the
# same DHCP client identifier and the LAN's DHCP server hands them
# all the same IPv4 lease — verified on rocket where 10 baked VMs
# with unique MACs all ended up with 192.168.1.169.
: > /etc/machine-id
rm -f /var/lib/dbus/machine-id
ln -sf /etc/machine-id /var/lib/dbus/machine-id
rm -f /var/lib/systemd/random-seed
rm -rf /var/lib/systemd/network/*
rm -rf /var/lib/dhcp/*
rm -rf /var/lib/dhcpcd/*
rm -f /etc/ssh/ssh_host_*

# Reset cloud-init state so a fresh boot from this image picks up the
# per-launch user-data.
cloud-init clean --logs
EOF

echo "==> stopping build instance"
incus stop --project "$INCUS_PROJECT" "$BUILD_NAME"

echo "==> publishing as ${IMAGE_ALIAS_VERSIONED}"
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

echo "==> deleting build instance"
incus delete --project "$INCUS_PROJECT" "$BUILD_NAME"

echo "==> done"
incus image list --project "$INCUS_PROJECT"
