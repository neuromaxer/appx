#!/usr/bin/env bash
# deploy/teardown.sh — reverse everything created by bootstrap.sh /
# system-setup.sh: stop+remove the systemd services, then optionally remove the
# appx/appx-agent users, the projects group, data directories, the env file, and
# the installed binaries.
#
# Must be run as root. Safe to run multiple times (idempotent).
#
# Shared build/runtime tools (go, node, pi, claude, uv) are intentionally left
# in place — they are not appx-specific. Pass --purge-data to also delete the
# data directory and the agent user's home (DESTRUCTIVE: removes all projects,
# the SQLite DB, TLS certs, and session transcripts).

set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
  echo "error: must run as root" >&2
  exit 1
fi

PURGE_DATA=0
for arg in "$@"; do
  case "$arg" in
    --purge-data) PURGE_DATA=1 ;;
    *) echo "unknown flag: $arg" >&2; exit 1 ;;
  esac
done

ENV_FILE="/etc/appx/appx.env"

# Resolve the data dir from the env file (fall back to the default) before we
# delete the env file.
DATA_DIR="/var/lib/appx"
if [ -f "$ENV_FILE" ]; then
  _APPX_DATA=$(grep '^APPX_DATA=' "$ENV_FILE" | cut -d= -f2- || true)
  if [ -n "$_APPX_DATA" ]; then
    DATA_DIR="${_APPX_DATA%/}"
  fi
fi

# ---------------------------------------------------------------------------
# 1. Systemd services
# ---------------------------------------------------------------------------

for svc in appx agent-server opencode; do
  if systemctl list-unit-files "$svc.service" >/dev/null 2>&1; then
    systemctl disable --now "$svc" 2>/dev/null || true
  fi
  rm -f "/etc/systemd/system/$svc.service"
done
systemctl daemon-reload
echo "removed services: appx, agent-server, opencode"

# Kill any stragglers that escaped systemd.
pkill -u appx-agent -f '(^|/)agent-server( |$)|agent-server/dist/server\.js' 2>/dev/null || true
pkill -u appx -f '(^|/)appx( |$)' 2>/dev/null || true

# ---------------------------------------------------------------------------
# 2. Binaries
# ---------------------------------------------------------------------------

rm -f /usr/local/bin/appx /usr/local/bin/agent-server /usr/local/bin/opencode
echo "removed binaries: /usr/local/bin/{appx,agent-server,opencode}"

# ---------------------------------------------------------------------------
# 3. Users and groups
# ---------------------------------------------------------------------------

# userdel -r would remove home dirs; we manage data deletion explicitly below so
# the default (no --purge-data) leaves project data on disk.
for user in appx appx-agent; do
  if id -u "$user" >/dev/null 2>&1; then
    userdel "$user" 2>/dev/null || true
    echo "removed user: $user"
  fi
done

for grp in projects appx-agent; do
  if getent group "$grp" >/dev/null 2>&1; then
    groupdel "$grp" 2>/dev/null || true
    echo "removed group: $grp"
  fi
done

# ---------------------------------------------------------------------------
# 4. Config + data
# ---------------------------------------------------------------------------

rm -f "$ENV_FILE"
# The seccomp profile is extracted from the agent image by tools-install.sh, so
# it is a generated artifact — remove it too, or /etc/appx never goes away.
# secrets.env is deliberately NOT removed unless --purge-data: it holds
# credentials the operator may not have stored elsewhere.
rm -f /etc/appx/seccomp-builder.json
if [ "$PURGE_DATA" -eq 1 ]; then
  rm -f /etc/appx/secrets.env
fi
rmdir /etc/appx 2>/dev/null || true
echo "removed config: $ENV_FILE and /etc/appx/seccomp-builder.json"
if [ "$PURGE_DATA" -eq 0 ] && [ -f /etc/appx/secrets.env ]; then
  echo "kept /etc/appx/secrets.env (provider credentials; --purge-data removes it)"
fi

if [ "$PURGE_DATA" -eq 1 ]; then
  rm -rf "$DATA_DIR" /home/appx-agent
  echo "purged data: $DATA_DIR and /home/appx-agent"
else
  echo "kept data: $DATA_DIR and /home/appx-agent (re-run with --purge-data to delete)"
fi

echo ""
echo "Teardown complete. Shared tools (go, node, pi, claude, uv) were left in place."
