// Package appx holds the single source of truth for which release of the
// appx-agent stack this appx build targets.
//
// appx consumes the agent stack only as published artifacts (see CLAUDE.md), and
// agent-server, agent-client and agent-protocol are versioned in lockstep by
// changesets — every appx-agent release bumps all three to the same number. So
// one version string pins the whole stack: the docker image appx supervises and
// the npm package its frontend bundles.
//
// That string lives in the AGENT_VERSION file rather than in this source file so
// the deploy shell scripts can read it too (they cannot import Go constants).
// This package embeds it, so the Go binary and the scripts cannot disagree.
//
// To move to a new agent release:
//
//	1. edit AGENT_VERSION
//	2. cd web && npm install   (updates package.json + the lockfile)
//	3. task test               (fails if step 2 was skipped)
//
// The web/package.json range is the one copy that cannot be eliminated — npm
// resolves dependencies only from package.json — so TestWebPackageJSON_MatchesAgentVersion
// asserts it agrees with this file.
package appx

import (
	_ "embed"
	"strings"
)

// agentVersionFile is the raw contents of AGENT_VERSION, including its trailing
// newline. Embedding requires the file to sit beside this source file, which is
// why this package lives at the repo root: go:embed rejects parent-directory
// patterns like "../AGENT_VERSION".
//
//go:embed AGENT_VERSION
var agentVersionFile string

// AgentVersion is the pinned appx-agent release (e.g. "0.1.7"), shared by the
// agent-server image and the @appx-org/agent-client npm package.
var AgentVersion = strings.TrimSpace(agentVersionFile)

// AgentImageRepo is the public registry repository for the agent-server image.
// It is amd64-only, which is why container-mode deploys require an amd64 host.
const AgentImageRepo = "ghcr.io/appx-org/agent-server"

// AgentImage is the fully-qualified image reference appx pulls and supervises by
// default (e.g. "ghcr.io/appx-org/agent-server:0.1.7"). Deployments can override
// it with APPX_AGENT_IMAGE — a tag or a @sha256: digest — without editing code.
var AgentImage = AgentImageRepo + ":" + AgentVersion
