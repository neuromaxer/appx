#!/usr/bin/env bash
# deploy/agent-version.sh — resolve the pinned appx-agent release.
#
# SOURCE this, don't execute it:
#
#   . "$SCRIPT_DIR/agent-version.sh"
#   echo "$AGENT_IMAGE"
#
# The version lives in the repo-root AGENT_VERSION file, which is also embedded
# into the appx binary (see agentversion.go). Reading it here rather than
# hardcoding a tag in each script is what keeps the deploy scripts, the Go
# default, and the generated /etc/appx/appx.env from drifting apart.
#
# Sets:
#   AGENT_VERSION — the pinned release (e.g. "0.1.7")
#   AGENT_IMAGE   — the full image ref (e.g. "ghcr.io/appx-org/agent-server:0.1.7")
#
# Callers that accept an override should do so themselves, e.g.
#   APPX_AGENT_IMAGE="${APPX_AGENT_IMAGE:-$AGENT_IMAGE}"

# Resolve the repo root from this file's location, so sourcing works regardless
# of the caller's cwd.
#
# BASH_SOURCE is bash-specific and unset under other shells (zsh, dash). Without
# this guard it expands empty, dirname yields ".", and we would silently read
# AGENT_VERSION from whatever directory the caller happened to be in — or from
# the parent of the repo. Fail loudly instead of pinning a wrong version.
if [ -z "${BASH_SOURCE[0]:-}" ]; then
  echo "ERROR: deploy/agent-version.sh must be sourced from bash" >&2
  echo "       (BASH_SOURCE is unset — are you using sh/zsh?)" >&2
  return 1 2>/dev/null || exit 1
fi

_AGENT_VERSION_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
_AGENT_VERSION_FILE="$_AGENT_VERSION_DIR/AGENT_VERSION"

if [ ! -f "$_AGENT_VERSION_FILE" ]; then
  echo "ERROR: $_AGENT_VERSION_FILE not found — cannot determine the pinned" >&2
  echo "       appx-agent release. Run this from a complete appx checkout." >&2
  exit 1
fi

# Strip whitespace/CR and ignore blank or commented lines.
AGENT_VERSION="$(grep -v '^[[:space:]]*#' "$_AGENT_VERSION_FILE" | tr -d '[:space:]')"

if [ -z "$AGENT_VERSION" ]; then
  echo "ERROR: $_AGENT_VERSION_FILE is empty." >&2
  exit 1
fi

# The published agent-server image. amd64-only, hence the amd64 deploy host
# requirement. Keep this repo in sync with agentversion.go's AgentImageRepo.
AGENT_IMAGE_REPO="ghcr.io/appx-org/agent-server"
AGENT_IMAGE="$AGENT_IMAGE_REPO:$AGENT_VERSION"

export AGENT_VERSION AGENT_IMAGE_REPO AGENT_IMAGE
