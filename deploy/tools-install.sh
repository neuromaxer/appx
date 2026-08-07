#!/usr/bin/env bash
# deploy/tools-install.sh — install build and runtime tools system-wide.
#
# Must be run as root. Safe to run multiple times (idempotent).
# Installs everything to /usr/local/bin so the appx user has access.
#
# Deploy is CONTAINER MODE ONLY: agent-server + Pi run INSIDE the appx-managed
# outer container, so this script does NOT install Pi or agent-server on the
# host. It PULLS the published agent-server image instead — appx depends on the
# released artifact from the appx-org/appx-agent monorepo, not on a sibling
# source checkout.
#
# Tools installed:
#   - Go          (version pinned to go.mod — builds the appx binary)
#   - Task        (taskfile.dev build runner — builds the appx binary)
#   - Node.js 24  (via nvm, pinned to major version — builds the appx web UI)
#   - the agent-server image (pulled, tag-pinned) + its seccomp profile
#
# Supported platforms: Ubuntu/Debian. NOTE: the appx binary builds on amd64 and
# arm64, but the published agent-server image is currently amd64-only, so a
# container-mode deploy needs an amd64 host.

set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
  echo "error: must run as root" >&2
  exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

# The published agent-server image appx is tested against. Keep in sync with
# containerruntime.DefaultImage (internal/containerruntime/config.go) and the
# APPX_AGENT_IMAGE default written by bootstrap.sh.
DEFAULT_AGENT_IMAGE="ghcr.io/appx-org/agent-server:0.1.6"

# Detect architecture.
ARCH=$(dpkg --print-architecture 2>/dev/null || echo "amd64")
case "$ARCH" in
  amd64) GO_ARCH="amd64" ;;
  arm64) GO_ARCH="arm64" ;;
  *) echo "ERROR: unsupported architecture: $ARCH"; exit 1 ;;
esac

# ---------------------------------------------------------------------------
# Task (taskfile.dev build runner)
# ---------------------------------------------------------------------------

if command -v task >/dev/null 2>&1; then
  echo "task already installed: $(task --version 2>/dev/null)"
else
  echo "installing task..."
  curl -1sLf 'https://dl.cloudsmith.io/public/task/task/setup.deb.sh' | bash
  apt-get install -y task
  echo "task installed: $(task --version 2>/dev/null)"
fi

# ---------------------------------------------------------------------------
# Go
# ---------------------------------------------------------------------------

# Read required version from go.mod; fall back to a known-good default.
GO_VERSION="1.24.2"
if [ -f "$REPO_DIR/go.mod" ]; then
  _GO_MOD_VER=$(grep '^go ' "$REPO_DIR/go.mod" | awk '{print $2}')
  if [ -n "$_GO_MOD_VER" ]; then
    GO_VERSION="$_GO_MOD_VER"
  fi
fi

if command -v go >/dev/null 2>&1; then
  echo "go already installed: $(go version 2>/dev/null)"
else
  echo "installing Go ${GO_VERSION}..."
  TMP_GO=$(mktemp)
  curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${GO_ARCH}.tar.gz" -o "$TMP_GO"
  rm -rf /usr/local/go
  tar -C /usr/local -xzf "$TMP_GO"
  rm -f "$TMP_GO"
  ln -sf /usr/local/go/bin/go /usr/local/bin/go
  ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt
  echo "go installed: $(go version 2>/dev/null)"
fi

# ---------------------------------------------------------------------------
# Node.js (via nvm, pinned to major version 24)
#
# nvm is installed to /usr/local/nvm (system-wide). Binaries are symlinked
# to /usr/local/bin so all users have access without sourcing nvm manually.
# ---------------------------------------------------------------------------

NODE_MAJOR=24
NVM_DIR="/usr/local/nvm"
NVM_VERSION="v0.40.1"

CURRENT_NODE_MAJOR=$(/usr/local/bin/node --version 2>/dev/null | sed 's/^v//' | cut -d. -f1 || echo "0")
if [ "$CURRENT_NODE_MAJOR" = "$NODE_MAJOR" ]; then
  echo "node $NODE_MAJOR already installed: $(/usr/local/bin/node --version)"
else
  echo "installing Node.js $NODE_MAJOR via nvm..."

  # Install nvm to system-wide location if not present.
  if [ ! -s "$NVM_DIR/nvm.sh" ]; then
    mkdir -p "$NVM_DIR"
    export NVM_DIR
    # PROFILE=/dev/null prevents nvm from modifying any shell profile file.
    curl -o- "https://raw.githubusercontent.com/nvm-sh/nvm/${NVM_VERSION}/install.sh" | \
      PROFILE=/dev/null bash
  fi

  # Source nvm for this script session.
  export NVM_DIR
  # shellcheck source=/dev/null
  . "$NVM_DIR/nvm.sh"

  # Install the pinned major version and symlink binaries system-wide.
  nvm install "$NODE_MAJOR"
  for bin in node npm npx; do
    ln -sf "$(nvm which $NODE_MAJOR | xargs dirname)/$bin" "/usr/local/bin/$bin"
  done

  echo "node installed: $(/usr/local/bin/node --version)"
fi

# Resolve the nvm bin directory where npm install -g puts binaries.
# Follow the /usr/local/bin/node symlink back to the nvm versioned directory.
NODE_BIN_DIR="$(dirname "$(readlink -f /usr/local/bin/node)")"

# ---------------------------------------------------------------------------
# Outer agent image — pulled from the registry. This is the only agent backend
# in container-mode deploy.
# ---------------------------------------------------------------------------

RUNTIME=""
command -v docker >/dev/null 2>&1 && RUNTIME="docker"
[ -z "$RUNTIME" ] && command -v podman >/dev/null 2>&1 && RUNTIME="podman"

# Pin the image. agent-server is published from the appx-org/appx-agent monorepo;
# appx consumes the published artifact and never builds it from source. Override
# APPX_AGENT_IMAGE to move to another tag or to pin by digest.
APPX_AGENT_IMAGE="${APPX_AGENT_IMAGE:-$DEFAULT_AGENT_IMAGE}"

if [ -z "$RUNTIME" ]; then
  echo "ERROR: no docker found — the outer runtime MUST be rootful host Docker." >&2
  echo "       Install it (apt-get install -y docker.io) and re-run." >&2
  exit 1
fi

echo "pulling outer image: $APPX_AGENT_IMAGE"
if ! "$RUNTIME" pull "$APPX_AGENT_IMAGE"; then
  echo "ERROR: failed to pull '$APPX_AGENT_IMAGE'." >&2
  echo "       The image is published publicly at ghcr.io/appx-org/agent-server;" >&2
  echo "       check network/DNS egress to ghcr.io, or set APPX_AGENT_IMAGE to a" >&2
  echo "       ref this host can reach. Note the image is currently amd64-only." >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Seccomp profile — extracted from the image we just pulled
# ---------------------------------------------------------------------------

# The tailored seccomp profile is the outer container's security boundary. It is
# a `docker run --security-opt seccomp=<host path>` argument, so it has to exist
# as a file on this host; appx references it by absolute path via
# APPX_AGENT_SECCOMP.
#
# We take it out of the image rather than vendoring a copy in this repo, so the
# profile appx applies is by construction the one the image was built with — a
# vendored duplicate can drift from the canonical version silently, and nothing
# would fail loudly if it did. The image bakes it at /opt/appx/.
SECCOMP_IN_IMAGE="/opt/appx/seccomp-builder.json"
SECCOMP_DEST="/etc/appx/seccomp-builder.json"

install -d -m 755 /etc/appx

# `docker create` makes a container without starting it — enough to copy a file
# out. Always remove it, including on the failure paths below.
if ! _seccomp_cid="$("$RUNTIME" create "$APPX_AGENT_IMAGE")" || [ -z "$_seccomp_cid" ]; then
  echo "ERROR: could not create a container from '$APPX_AGENT_IMAGE' to extract" >&2
  echo "       the seccomp profile. Is the docker daemon healthy?" >&2
  exit 1
fi

_seccomp_ok=0
if "$RUNTIME" cp "$_seccomp_cid:$SECCOMP_IN_IMAGE" "$SECCOMP_DEST.tmp" 2>/dev/null &&
  [ -s "$SECCOMP_DEST.tmp" ]; then
  _seccomp_ok=1
fi
"$RUNTIME" rm "$_seccomp_cid" >/dev/null 2>&1 || true

if [ "$_seccomp_ok" -eq 1 ]; then
  install -m 644 "$SECCOMP_DEST.tmp" "$SECCOMP_DEST"
  rm -f "$SECCOMP_DEST.tmp"
  echo "extracted seccomp profile from image → $SECCOMP_DEST"
else
  rm -f "$SECCOMP_DEST.tmp"
  echo "ERROR: '$APPX_AGENT_IMAGE' does not ship $SECCOMP_IN_IMAGE." >&2
  echo "       appx cannot start the outer container without the tailored profile" >&2
  echo "       (docker's default seccomp blocks mount(2) and breaks nested" >&2
  echo "       rootless podman; seccomp=unconfined is not an acceptable" >&2
  echo "       substitute). Use an agent-server image that bakes it in —" >&2
  echo "       $DEFAULT_AGENT_IMAGE or newer." >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------

echo ""
echo "Tools install complete."
echo ""
echo "  task:     $(task --version 2>/dev/null || echo 'not found')"
echo "  go:       $(go version 2>/dev/null || echo 'not found')"
echo "  node:     $(/usr/local/bin/node --version 2>/dev/null || echo 'not found')"
echo "  outer image ($APPX_AGENT_IMAGE): $("$RUNTIME" image inspect "$APPX_AGENT_IMAGE" >/dev/null 2>&1 && echo present || echo 'not found')"
echo "  seccomp profile: $([ -f "$SECCOMP_DEST" ] && echo "$SECCOMP_DEST" || echo 'not found')"
