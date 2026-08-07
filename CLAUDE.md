# Appx

Agentic Application Proxy: a self-hostable tool to build and host personal apps with AI agents powered by Pi.

## Quick Reference

```bash
task local          # Build and run appx in HTTP dev mode (127.0.0.1.sslip.io, port 8080)
task build          # Build frontend + Go binary -> ./appx (without running)
task web            # Build frontend only, copy to cmd/appx/web/dist
task test           # Run all Go tests
task lint           # Lint frontend
task clean          # Remove build artifacts
./appx -port 8443   # Run (default port 443, requires root)
```

## Architecture

Single Go binary serves everything on one port (HTTPS or HTTP in dev mode). Pi runs behind the `agent-server` service on `localhost:4001`. agent-server owns project identity, the on-disk project directory (including each project's `.pi/` harness), session transcripts, models, and credentials; appx is a **control plane + authorizing gateway** that owns auth, TLS, port/subdomain assignment, egress policy, and a per-project SQLite record, and proxies agent traffic to agent-server. See `.superpowers/specs/2026-06-09-project-ownership-and-agent-chat-integration-adr.md` (written when the SDK was still called agent-chat).

### Dependency on the agent stack

The agent stack lives in the separate [appx-agent](https://github.com/appx-org/appx-agent) monorepo and is consumed **only as published artifacts** — never as sibling checkouts or vendored copies:

| Artifact | Consumed as |
| --- | --- |
| `agent-server` | `ghcr.io/appx-org/agent-server` docker image (public, amd64-only) |
| `@appx-org/agent-client` | public npm package (ships compiled `dist/`) |
| `@appx-org/agent-protocol` | public npm, transitive via agent-client |

**`AGENT_VERSION` (repo root) is the single source of truth for all three.** changesets versions them in lockstep, so one string pins the whole stack. Never hardcode an agent version or image tag anywhere else:

- **Go** — `agentversion.go` (package `appx`, at the repo root so `go:embed` can reach the file; `..` patterns are rejected) embeds it and exposes `AgentVersion`/`AgentImage`. `containerruntime.DefaultImage` derives from it.
- **Shell** — source `deploy/agent-version.sh`, which sets `$AGENT_VERSION`/`$AGENT_IMAGE`. Requires bash (it uses `BASH_SOURCE`) and fails loudly under sh/zsh rather than silently resolving the wrong path.
- **Docs** — use `$(cat AGENT_VERSION)` in copy-pasteable commands.
- **npm** — `web/package.json` is the one unavoidable copy, since npm resolves only from `package.json`.

To upgrade: edit `AGENT_VERSION`, then `cd web && npm install`, then `task test`. Three tests enforce this: `TestDefaultImage_DerivesFromAgentVersion`, `TestNoHardcodedImageTagsInShell` (rejects a pasted tag in any deploy script), and `TestAgentVersion_MatchesWebPackageJSON` (fails if you skip the `npm install`). Pin a semver tag or digest — never `latest`/`edge`, which would make deploys irreproducible.

The outer container's **tailored seccomp profile** is a `docker run --security-opt` argument, so it must be a host file. `deploy/tools-install.sh` extracts it from the pulled image (`/opt/appx/seccomp-builder.json` → `/etc/appx/seccomp-builder.json`) rather than vendoring a copy, so the applied profile is always the one the image was built with. Do not reintroduce a checked-in copy.

- `localhost:<port>`: appx dashboard, embedded React SPA, and REST API.
- `/api/*`: public `POST /api/login`, protected everything else.
- `/api/pi/*`: same-origin 1:1 mirror of the agent-server `/v1` contract, consumed by the `@appx-org/agent-client` SDK. Authorizes project-scoped session traffic against the caller's registered projects (by slug) and never exposes project-lifecycle routes.
- `/api/projects/:id/agent/*`: legacy project-scoped Pi session proxy (no remaining frontend consumer; retained pending cleanup).
- `/api/agent/*`: shared Pi provider auth, subscription login, model, and custom provider proxy.
- `<project-name>.<base-domain>`: reverse proxy to agent-built apps on assigned ports.

Auth uses a single-user password login with an `appx_session` cookie, bcrypt password hashing, rate-limited login, and 30-day sessions. TLS uses generated self-signed certificates by default or Let's Encrypt with Cloudflare DNS-01 when configured.

## Project Structure

```text
AGENT_VERSION                  # Pinned appx-agent release — single source of truth
agentversion.go                # package appx: embeds AGENT_VERSION (root so go:embed can reach it)
cmd/appx/main.go               # Entry point, CLI flags, dependency wiring
internal/
  agentserver/
    client.go                  # Client for agent-server project lifecycle (EnsureProject/DeleteProject)
  auth/
    auth.go                    # Auth struct, middleware, session cookie helpers
    store.go                   # Password + session CRUD, generic key-value settings
  db/
    db.go                      # SQLite connection and migration runner
    migrations/                # Numbered up/down SQL files
  egress/
    proxy.go                   # Go CONNECT proxy for agent egress control
    store.go                   # Allowlist and connection log persistence
  project/
    manager.go                 # Project lifecycle: register name with agent-server + appx record (no filesystem scaffolding)
    store.go                   # Project CRUD, port assignment, status transitions
  server/
    router.go                  # Route registration, SPA handler, subdomain proxy
    agent_proxy.go             # agent-server reverse proxies: /api/pi mirror, project-scoped, and global
    agent_handlers.go          # Pi provider auth and custom-provider handlers
    project_handlers.go        # Project CRUD and app health shape
    settings_handlers.go       # Account and app settings
    shell_handlers.go          # Local PTY shell endpoints
  terminal/
    local.go                   # Local PTY sessions for server/project terminals
  tls/
    selfsigned.go              # Self-signed certificate generation
web/src/
  api/client.ts                # Typed Appx API client
  pages/Project.tsx            # Agent (via @appx-org/agent-client) and terminal tabs
  components/Terminal.tsx      # xterm.js wrapper for local PTY shell
  pages/Dashboard.tsx          # Project list
  pages/Project.tsx            # Agent and terminal tabs
  pages/Settings.tsx           # Pi credentials, subscriptions, custom providers
deploy/
  appx.service                 # systemd unit for appx (container mode; ordered after docker.service)
  bootstrap.sh                 # Full install/update flow (container mode only)
  system-setup.sh              # appx user, projects group, dirs, /etc/appx, docker group, unit
  tools-install.sh             # Go, Node.js, Task, + pulls the agent image & extracts its seccomp profile
  agent-version.sh             # Sourced by the above: AGENT_VERSION -> $AGENT_IMAGE
```

## Tech Stack

- Backend: Go 1.26, stdlib `net/http`, `database/sql` with `modernc.org/sqlite`.
- Frontend: React 19, Vite 8, TypeScript 5.9, react-router-dom 7.
- Agent runtime: Pi CLI plus `agent-server`, from the published `ghcr.io/appx-org/agent-server` image — run inside the appx-managed outer container (production) or by `docker run` for local dev.
- Streaming: Appx frontend consumes the agent-server HTTP/SSE session contract.
- Markdown: rendered inside agent-client (`marked` + `dompurify` are its dependencies, not appx's).
- Deployment: Task, systemd. **Container-mode only:** appx runs as the `appx` OS user (in the `projects` + `docker` groups) and supervises the agent-server outer container; the agent runs as an unprivileged uid *inside* that container, so there is no `appx-agent` host user.

## Conventions

### Go

- Every exported and unexported function, method, and type should have a useful doc comment. For handlers, document method/path, request/response shape, and auth requirements.
- Use dependency injection through parameters or config structs, not package globals.
- Keep handlers as `http.HandlerFunc` closures.
- Tests use in-memory SQLite (`:memory:`) and `httptest`.
- Wrap errors with context using `fmt.Errorf("context: %w", err)`.

### Frontend

- Every exported function and component should have a JSDoc comment.
- Keep endpoint calls in `web/src/api/client.ts`.
- Use the existing dark Appx design tokens from `web/src/index.css`; avoid one-off hardcoded colors unless a component already does so.
- Agent chat UI is provided by the `@appx-org/agent-client` npm package. It talks to the `/api/pi` mirror; do not reintroduce a hand-written session store/reducer, and do not re-link it to a local checkout in committed code (see `docs/readme/local-development.md` for the `npm link` dev override). Re-theme via the `--ac-*` token bridge in `web/src/index.css`.
- On 401, redirect to `/login` via `redirectToLogin()` from `api/client.ts` — assigning `window.location` inline inside a hook trips `react-hooks/immutability`.

### Build

- Use Task targets from `Taskfile.yml`.
- `task build` builds the frontend, copies `web/dist` into `cmd/appx/web/dist`, then builds Go.
- The frontend is embedded in the Go binary via `go:embed`.

## Verification Loop

For any code change, run the narrowest useful checks while iterating and finish with:

```text
[ ] task build
[ ] task test
[ ] task lint
[ ] Manual verification for affected UI/API/deploy behavior
```

Add or update tests when behavior changes, especially for server routes, database migrations, store methods, and regression fixes.

## Deployment Notes

- `deploy/bootstrap.sh` is first-run setup (container mode only: appx as the `appx` systemd service supervising the agent-server outer container). It needs no sibling checkouts — the agent image is pulled and the SDK comes from npm.
- `task server:deploy` pulls code, rebuilds, installs, re-pulls the pinned agent image (re-extracting its seccomp profile), restarts `appx`, then verifies.
- Deploy hosts must be **amd64**: the published agent-server image is amd64-only and is no longer built on the box.
- The agent (agent-server + Pi) runs as an unprivileged uid **inside** the outer container, not as a host user; provider secrets reach it via the service env (`/etc/appx/secrets.env`, `root:root 0600`), forwarded into the container by name.
- The Docker daemon (`--restart unless-stopped`) keeps the outer container alive across crash + reboot; appx's startup `EnsureRunning` re-attaches idempotently (never auto-recreates on drift). `appx.service` is ordered `After=docker.service`.
- `appx` is in the `docker` group (root-equivalent — accepted on a dedicated box; Stage 5 scopes it down). It binds 443 via `CAP_NET_BIND_SERVICE`, not root.
- Provider traffic from Pi goes through the Appx egress proxy (bridge gateway in container mode); loopback traffic to agent-server stays local.
- The HSTS header includes `includeSubDomains`. Do not point appx at a shared domain that also hosts HTTP services on subdomains.

## Documentation

After implementing behavior changes, update `README.md`, this file, and any current docs that describe the changed behavior. Historical planning documents may describe old phases, but active development docs should reflect the current Pi-only architecture.

## Superpowers

All brainstorm specs, implementation plans, and design documents generated by superpowers skills go in `.superpowers/specs/`. Use the naming convention `YYYY-MM-DD-<topic>-<type>.md`.
