#!/usr/bin/env bash
# incuse installer.
#
# Idempotent: re-running it on an already-installed host upgrades the
# binary + unit and restarts the service without churn elsewhere.
#
# Required env / args:
#   $1                — path to the incuse binary to install (default: ./bin/incuse)
#
# Optional env:
#   INCUSE_PREFIX     — install prefix (default /usr/local)
#   INCUSE_CONFIG_DIR — config directory (default /etc/incuse)
#   INCUSE_USER       — service user (default incuse)
#   INCUSE_GROUP      — service group (default incuse)

set -euo pipefail

BIN_SRC="${1:-./bin/incuse}"
PREFIX="${INCUSE_PREFIX:-/usr/local}"
CONFIG_DIR="${INCUSE_CONFIG_DIR:-/etc/incuse}"
SERVICE_USER="${INCUSE_USER:-incuse}"
SERVICE_GROUP="${INCUSE_GROUP:-incuse}"

if [[ $EUID -ne 0 ]]; then
	echo "install.sh: must run as root" >&2
	exit 1
fi

if [[ ! -f "$BIN_SRC" ]]; then
	echo "install.sh: binary not found at $BIN_SRC" >&2
	echo "  build first: make build" >&2
	exit 1
fi

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &> /dev/null && pwd)"
unit_src="$script_dir/incuse.service"
example_src="$script_dir/incuse.example.yaml"

# 1. Service user/group with `incus-admin` supplementary group.
if ! getent group "$SERVICE_GROUP" > /dev/null; then
	groupadd --system "$SERVICE_GROUP"
fi
if ! getent passwd "$SERVICE_USER" > /dev/null; then
	useradd \
		--system \
		--gid "$SERVICE_GROUP" \
		--home-dir /var/lib/incuse \
		--shell /usr/sbin/nologin \
		--comment "incuse orchestrator" \
		"$SERVICE_USER"
fi
if getent group incus-admin > /dev/null; then
	usermod -aG incus-admin "$SERVICE_USER"
else
	echo "install.sh: WARNING: group incus-admin missing — install Incus before incuse" >&2
fi

# 2. Binary.
install -d -m 0755 "$PREFIX/bin"
install -m 0755 "$BIN_SRC" "$PREFIX/bin/incuse"

# 3. Config directory + example. Don't clobber an existing config.
install -d -m 0750 -o root -g "$SERVICE_GROUP" "$CONFIG_DIR"
install -m 0640 -o root -g "$SERVICE_GROUP" "$example_src" "$CONFIG_DIR/config.example.yaml"
if [[ ! -f "$CONFIG_DIR/config.yaml" ]]; then
	install -m 0640 -o root -g "$SERVICE_GROUP" "$example_src" "$CONFIG_DIR/config.yaml"
	echo "install.sh: dropped example config at $CONFIG_DIR/config.yaml — edit before starting"
fi

# 4. State directory. systemd recreates this on Start via
# StateDirectory=, but pre-create so nothing in the unit assumes a
# specific run order.
install -d -m 0750 -o "$SERVICE_USER" -g "$SERVICE_GROUP" /var/lib/incuse

# 5. Systemd unit.
install -m 0644 "$unit_src" /etc/systemd/system/incuse.service
systemctl daemon-reload

echo
echo "install.sh: done."
echo "  next: edit $CONFIG_DIR/config.yaml, drop a chmod-600 PAT at"
echo "        $CONFIG_DIR/github.pat (or wire LoadCredential), then:"
echo
echo "          systemctl enable --now incuse"
echo "          journalctl -u incuse -f"
