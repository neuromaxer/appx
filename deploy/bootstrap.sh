#!/usr/bin/env bash
# deploy/bootstrap.sh — full setup from repo checkout to running services.
#
# Usage: sudo ./deploy/bootstrap.sh
#
# On first run, prompts for server configuration (IP, data dir, port)
# and writes /etc/appx/appx.env. On subsequent runs, skips the prompt
# and uses the existing config.
#
# Requires: go, node/npm, task (taskfile.dev) on the server.
# Safe to run multiple times.

set -euo pipefail

STEP=""
trap 'if [ -n "$STEP" ]; then echo ""; echo "FAILED at: $STEP"; fi' ERR

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
ENV_FILE="/etc/appx/appx.env"

# ---------------------------------------------------------------------------
# 1. Server configuration (interactive on first run only).
# ---------------------------------------------------------------------------

STEP="config"
if [ -f "$ENV_FILE" ]; then
  echo "using existing config: $ENV_FILE"
else
  echo "=== Server Configuration ==="
  echo ""
  echo "  Hostname: uses sslip.io by default (free wildcard DNS) so subdomain"
  echo "  routing works for agent-built apps. Use your own domain if you have one."
  echo ""
  echo "  Examples:"
  echo "    138.199.158.226.sslip.io    (default — subdomain routing works)"
  echo "    app.example.com             (custom domain — set APPX_DOMAIN later for Let's Encrypt)"
  echo ""
  echo "  Data directory: stores the DB, TLS certs, and project files."
  echo "  Use a mounted volume path if your root disk is small."
  echo ""

  # Auto-detect public IP and default to sslip.io hostname for subdomain support.
  DEFAULT_IP=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{print $7; exit}' || echo "")
  DEFAULT_HOST=""
  if [ -n "$DEFAULT_IP" ]; then
    DEFAULT_HOST="${DEFAULT_IP}.sslip.io"
  fi

  read -rp "Server hostname [${DEFAULT_HOST:-none detected}]: " INPUT_HOST
  APPX_HOST="${INPUT_HOST:-$DEFAULT_HOST}"

  read -rp "Data directory [/var/lib/appx]: " INPUT_DATA
  APPX_DATA="${INPUT_DATA:-/var/lib/appx}"
  APPX_DATA="${APPX_DATA%/}"

  read -rp "Port [443]: " INPUT_PORT
  APPX_PORT="${INPUT_PORT:-443}"

  echo ""

  # Write env file.
  mkdir -p /etc/appx
  cat > "$ENV_FILE" <<EOF
# Appx server configuration — created by bootstrap.
# Edit and restart: sudo nano /etc/appx/appx.env && sudo systemctl restart appx
#
# Examples:
#
#   Default (sslip.io — free wildcard DNS, enables subdomain routing):
#     APPX_HOST=138.199.158.226.sslip.io
#     APPX_DATA=/var/lib/appx
#     APPX_PORT=443
#
#   Server with mounted volume:
#     APPX_HOST=138.199.158.226.sslip.io
#     APPX_DATA=/mnt/vol/appx-data
#     APPX_PORT=443
#
#   Custom domain with Let's Encrypt:
#     APPX_HOST=138.199.158.226.sslip.io
#     APPX_DATA=/var/lib/appx
#     APPX_PORT=443
#     APPX_DOMAIN=app.example.com
#     CLOUDFLARE_API_TOKEN=your_token_here
#     APPX_AGENT_SERVER_URL=http://127.0.0.1:4001
#
# All variables:
#   APPX_HOST   — server hostname for TLS cert and routing (default: <ip>.sslip.io)
#   APPX_DATA   — data directory: DB, TLS certs, projects (default: /var/lib/appx)
#   APPX_PORT   — listen port (default: 443). MUST be open in firewall
#   APPX_AGENT_SERVER_URL — Pi agent-server URL used by the Appx proxy
#   APPX_DOMAIN — domain for Let's Encrypt via Cloudflare DNS-01 (optional)
#   CLOUDFLARE_API_TOKEN — Cloudflare API token for DNS-01 challenge (optional)
#   APPX_AGENT_CONTAINER — always "true": appx creates/supervises the
#                          agent-server OUTER container (the only deploy mode)
#   APPX_AGENT_IMAGE — published agent-server image ref to pull (tag or digest)
#   APPX_AGENT_SECCOMP — absolute path to the tailored seccomp profile
#                        (tools-install.sh extracts it from the image to
#                        /etc/appx/seccomp-builder.json)
#   APPX_AGENT_MEMORY  — outer container memory ceiling (default 4g). The agent
#                        runs model-authored builds, so this is on by default;
#                        raise it if builds are SIGKILLed. "unlimited" opts out.
#   APPX_AGENT_CPUS    — outer container CPU ceiling (default 2.0). Same
#                        semantics, including "unlimited".
#   APPX_AGENT_ENV_PASSTHROUGH — comma-separated env var NAMES forwarded by name
#                          into the container, for creds that must come from the
#                          service env rather than the Settings UI (e.g. Bedrock).
#                          Most providers (incl. Anthropic) are configured in the
#                          Settings UI instead.

APPX_HOST=$APPX_HOST
APPX_DATA=$APPX_DATA
APPX_PORT=$APPX_PORT
APPX_AGENT_SERVER_URL=http://127.0.0.1:4001
# APPX_DOMAIN=
# CLOUDFLARE_API_TOKEN=

# --- Container mode (the only deploy path): appx manages the outer container ---
APPX_AGENT_CONTAINER=true
# The published agent-server image (appx-org/appx-agent monorepo). Keep in sync
# with tools-install.sh's DEFAULT_AGENT_IMAGE and containerruntime.DefaultImage.
APPX_AGENT_IMAGE=ghcr.io/appx-org/agent-server:0.1.7
APPX_AGENT_SECCOMP=/etc/appx/seccomp-builder.json
# Resource ceiling for the outer container. Defaults (4g / 2.0) are applied in
# code, so these stay commented unless this box needs different values. Node plus
# nested podman builds want headroom — if a build dies near the ceiling, raise
# memory rather than lowering it. Set to "unlimited" to omit the flag entirely.
# APPX_AGENT_MEMORY=4g
# APPX_AGENT_CPUS=2.0
# Provider credentials: configure them in the Settings UI (stored in the agent's
# Pi credential storage, persisted in the builder-workspace volume) — this is the
# path for Anthropic and most providers. ONLY creds that the Settings UI can't
# carry (e.g. Amazon Bedrock's AWS_BEARER_TOKEN_BEDROCK, an upstream Pi gap) need
# the env path: put them in /etc/appx/secrets.env (root:root 0600) and list the
# var NAMES here so appx forwards them into the container by name (never baked):
#   APPX_AGENT_ENV_PASSTHROUGH=AWS_BEARER_TOKEN_BEDROCK,AWS_REGION
APPX_AGENT_ENV_PASSTHROUGH=AWS_BEARER_TOKEN_BEDROCK,AWS_REGION
EOF
  chmod 600 "$ENV_FILE"
  echo "wrote config → $ENV_FILE"
fi

echo ""

# ---------------------------------------------------------------------------
# 2. System setup: users, groups, directories, service files.
# ---------------------------------------------------------------------------

STEP="system-setup"
"$SCRIPT_DIR/system-setup.sh"

echo ""

# ---------------------------------------------------------------------------
# 3. Install tools: build toolchain, terminal tools, + the outer image.
# ---------------------------------------------------------------------------

STEP="tools-install"
"$SCRIPT_DIR/tools-install.sh"

echo ""

# ---------------------------------------------------------------------------
# 4. Build the appx binary (or use pre-built).
# ---------------------------------------------------------------------------

STEP="build"
# Always rebuild from source when tools are available so that direct invocations
# of this script (not via `task server:bootstrap`) don't silently reuse a stale
# binary. `task build` is fast when nothing has changed (file-based caching).
if command -v task >/dev/null 2>&1; then
  echo "building appx..."
  cd "$REPO_DIR" && task build
  echo "build complete"
elif command -v go >/dev/null 2>&1 && command -v node >/dev/null 2>&1; then
  echo "building appx (task not found, falling back to manual build)..."
  cd "$REPO_DIR"
  cd web && npm install && npm run build && cd "$REPO_DIR"
  rm -rf cmd/appx/web/dist && mkdir -p cmd/appx/web && cp -r web/dist cmd/appx/web/dist
  go build -o appx ./cmd/appx
  echo "build complete"
elif [ -f "$REPO_DIR/appx" ]; then
  echo "build tools not available — using pre-built binary (run deploy/tools-install.sh to install them)"
else
  echo "ERROR: no appx binary found and no build tools available."
  echo ""
  echo "  Run deploy/tools-install.sh first, or copy a pre-built binary to $REPO_DIR/appx."
  exit 1
fi

echo ""

# ---------------------------------------------------------------------------
# 5. Install the appx binary.
# ---------------------------------------------------------------------------

STEP="install-binary"
install -m 750 -o root -g appx "$REPO_DIR/appx" /usr/local/bin/appx
echo "installed appx binary → /usr/local/bin/appx"

echo ""

# ---------------------------------------------------------------------------
# 6. Restart services.
# ---------------------------------------------------------------------------

STEP="restart-services"
# Container mode is the only deploy path: appx creates/supervises the outer
# container (which runs agent-server) at boot. There is no host
# agent-server.service to start.
echo "stopping services..."
systemctl stop appx 2>/dev/null || true
sleep 2
echo "starting appx (it will EnsureRunning the outer container)..."
systemctl start appx
echo "waiting for agent-server inside the container (published on 127.0.0.1:4001)..."
for i in $(seq 1 60); do
  curl -sf http://127.0.0.1:4001/ >/dev/null 2>&1 && break
  sleep 2
done
echo "services started"

echo ""

# ---------------------------------------------------------------------------
# 7. Verify everything is correct.
# ---------------------------------------------------------------------------

STEP="verify"
"$SCRIPT_DIR/verify-installation.sh"
STEP=""

# ---------------------------------------------------------------------------
# Done.
# ---------------------------------------------------------------------------

APPX_HOST_VAL=$(grep '^APPX_HOST=' "$ENV_FILE" | cut -d= -f2- || true)
APPX_PORT_VAL=$(grep '^APPX_PORT=' "$ENV_FILE" | cut -d= -f2- || true)
APPX_PORT_VAL="${APPX_PORT_VAL:-443}"

echo ""
echo "========================================"
echo "  Setup complete!"
echo ""
if [ -n "$APPX_HOST_VAL" ]; then
  if [ "$APPX_PORT_VAL" = "443" ]; then
    echo "  Visit: https://${APPX_HOST_VAL}"
  else
    echo "  Visit: https://${APPX_HOST_VAL}:${APPX_PORT_VAL}"
  fi
fi
echo "  Open Settings to configure Pi credentials and models."
echo "========================================"
