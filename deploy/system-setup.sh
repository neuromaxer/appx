#!/usr/bin/env bash
# deploy/system-setup.sh — create OS users, groups, directories, and install the
# appx systemd service.
#
# Must be run as root. Safe to run multiple times (idempotent).
#
# Deploy is CONTAINER MODE ONLY: appx runs as the `appx` systemd service and
# creates/supervises the agent-server OUTER container itself (one unprivileged
# container holding agent-server + rootless podman). There is no host
# `appx-agent` user, no host `agent-server.service`, and no host install of
# Node/Pi/agent-server. Local development does not use this script — a developer
# runs the agent-server image by hand and `appx --http` with
# APPX_AGENT_SERVER_URL.
#
# What this script does:
#   1. Reads APPX_DATA from /etc/appx/appx.env (falls back to /var/lib/appx)
#   2. Creates the appx user (home = data dir) and the shared projects group
#   3. Sets up directories with correct ownership and permissions
#   4. Creates /etc/appx for config + the extracted seccomp profile
#   5. Adds appx to the docker group so the service can drive the daemon
#   6. Copies the appx systemd service file and enables it
#
# What this script does NOT do:
#   - Install Go, Node, or pull the agent-server image (use tools-install.sh)
#   - Install the seccomp profile (tools-install.sh extracts it from the image)
#   - Copy the appx binary (handled by bootstrap.sh / server:deploy)

set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
  echo "error: must run as root" >&2
  exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# ---------------------------------------------------------------------------
# Data directory — read early so it can be used for the appx user's home dir.
# ---------------------------------------------------------------------------

DATA_DIR="/var/lib/appx"
if [ -f /etc/appx/appx.env ]; then
  # shellcheck source=/dev/null
  _APPX_DATA=$(grep '^APPX_DATA=' /etc/appx/appx.env | cut -d= -f2- || true)
  if [ -n "$_APPX_DATA" ]; then
    DATA_DIR="${_APPX_DATA%/}"
  fi
fi
echo "data directory: $DATA_DIR"
echo "agent backend: pi (container mode — appx supervises the outer container)"

# ---------------------------------------------------------------------------
# OS users and groups
# ---------------------------------------------------------------------------

# Shared group — appx (and, inside the container, the agent uid) reach project
# directories through this group.
if ! getent group projects >/dev/null 2>&1; then
  groupadd --system projects
  echo "created group: projects"
else
  echo "group projects already exists"
fi

# appx user — runs the appx server process.
# Home dir is the data directory so that shell sessions started by the terminal
# feature land in the right place and have access to tools in PATH.
if ! id -u appx >/dev/null 2>&1; then
  useradd --system --create-home --shell /bin/bash --home-dir "$DATA_DIR" \
    --groups projects appx
  echo "created user: appx (home: $DATA_DIR)"
else
  # Ensure existing user has correct shell, group membership, and home dir.
  CURRENT_HOME=$(getent passwd appx | cut -d: -f6)
  usermod --shell /bin/bash --append --groups projects appx || true
  if [ "$CURRENT_HOME" != "$DATA_DIR" ]; then
    usermod --home "$DATA_DIR" appx
    echo "user appx already exists (updated home: $CURRENT_HOME → $DATA_DIR)"
  else
    echo "user appx already exists (updated shell and groups)"
  fi
fi

# ---------------------------------------------------------------------------
# Directories
# ---------------------------------------------------------------------------

# Data dir: appx owns it. Accessible for traversal by the agent user so it can
# reach the projects/ subdirectory inside it.
install -d -o appx -g appx -m 755 "$DATA_DIR"
echo "directory ready: $DATA_DIR (appx:appx 755)"

# Internals subdir: DB, TLS certs, password, AGENT_SERVER_TOKEN — appx-only.
install -d -o appx -g appx -m 700 "$DATA_DIR/.appx-internals"
echo "directory ready: $DATA_DIR/.appx-internals (appx:appx 700)"

# Projects subdir: shared workspace. Setgid ensures new files inherit the
# projects group.
install -d -o appx -g projects -m 2770 "$DATA_DIR/projects"
echo "directory ready: $DATA_DIR/projects (appx:projects 2770)"

# ---------------------------------------------------------------------------
# Container mode: config dir + docker access
# ---------------------------------------------------------------------------

# /etc/appx holds appx.env, secrets.env, and the seccomp profile. The profile
# itself is NOT installed here: it is extracted from the agent-server image by
# tools-install.sh (which runs after this script), so that the profile appx
# applies is always the one the pulled image was built with.
install -d -m 755 /etc/appx

if ! command -v docker >/dev/null 2>&1; then
  echo "WARNING: docker is not installed. The outer runtime MUST be rootful host"
  echo "         Docker (validated by the spike: rootless docker breaks nested"
  echo "         rootless podman). Install it, e.g.: apt-get install -y docker.io"
fi

# Docker access for the appx user (DECIDED — see phase_9_plan.md Stage 4 / Risk
# #4). The outer runtime is rootful host Docker (rootless-docker-outer is
# non-viable for the nested-podman workload), so the unprivileged `appx` service
# user reaches the root daemon via the `docker` group. NOTE: docker-group
# membership is ROOT-EQUIVALENT (`docker run -v /:/host` owns the box); accepted
# here on a dedicated single-purpose box + dedicated appx user. Scoping it down
# (docker-socket proxy / narrow sudoers) is Stage 5 hardening — do NOT attempt
# rootless. Under systemd User=appx the service inherits the group after
# daemon-reload + restart.
if getent group docker >/dev/null 2>&1; then
  if id -nG appx 2>/dev/null | grep -qw docker; then
    echo "appx already in the docker group"
  else
    usermod --append --groups docker appx || true
    echo "added appx to the 'docker' group (root-equivalent — Stage 5 scopes it down)"
  fi
else
  echo "WARNING: no 'docker' group present — install rootful docker so appx can drive the daemon"
fi

# ---------------------------------------------------------------------------
# Appx binary permissions (if binary already deployed)
# ---------------------------------------------------------------------------

if [ -f /usr/local/bin/appx ]; then
  chown root:appx /usr/local/bin/appx
  chmod 750 /usr/local/bin/appx
  echo "binary permissions: /usr/local/bin/appx (root:appx 750)"
fi

# ---------------------------------------------------------------------------
# Systemd service file
# ---------------------------------------------------------------------------

cp "$SCRIPT_DIR/appx.service" /etc/systemd/system/appx.service
echo "copied appx.service"

systemctl daemon-reload
echo "systemd reloaded"

systemctl enable appx
echo "service enabled: appx (agent-server runs inside the appx-managed container)"

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------

echo ""
echo "System setup complete."
