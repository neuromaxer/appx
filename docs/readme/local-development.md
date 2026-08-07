# Local Development

Local development does **not** use the deploy scripts, systemd, or appx's
container supervision. You run the published `agent-server` image yourself and
point appx at it with `APPX_AGENT_SERVER_URL`; appx runs as a plain process with
`--http`.

Everything appx depends on is a published artifact, so there is nothing to check
out alongside this repo:

- `@appx-org/agent-client` (and its `@appx-org/agent-protocol` dependency) come
  from the public npm registry and are installed by `task web` / `task build`
  like any other dependency.
- `agent-server` comes from `ghcr.io/appx-org/agent-server`, which is publicly
  pullable — no registry login required.

Both are released from the
[appx-agent](https://github.com/appx-org/appx-agent) monorepo.

## Run agent-server

Point the container's `/workspace` at the **same** projects directory appx uses.
agent-server owns the project directories, and appx's subdomain proxy and
terminal read them from that shared path, so the two must agree.

Run it from the repo root so `AGENT_VERSION` resolves — that file is the single
source of truth for which agent release this appx checkout targets, so the
command below always matches what a deploy would pull:

```bash
mkdir -p ~/appx-data/projects

docker run --rm -it \
  --name agent-server-dev \
  -p 127.0.0.1:4001:4001 \
  -v ~/appx-data/projects:/workspace \
  "ghcr.io/appx-org/agent-server:$(cat AGENT_VERSION)"
```

The image is amd64-only today, so on Apple Silicon add `--platform linux/amd64`
to run it emulated — slower, but fine for driving the appx UI.

> The container-mode flags appx uses in production (rootless-podman nesting, the
> tailored seccomp profile, egress proxying) are **not** needed here. They exist
> so the agent can build and run *inner* app containers; a plain `docker run` is
> enough for working on appx itself.

To iterate on agent-server's own code, clone the monorepo and run
`npm run dev -w packages/agent-server` with `WORKSPACE_DIR` set instead of using
the image.

## Run appx

```bash
APPX_DATA=~/appx-data task local
```

`task local` builds and runs appx with `--host 127.0.0.1.sslip.io` against
`APPX_AGENT_SERVER_URL` (default `http://127.0.0.1:4001`), listening on port
8080. `APPX_DATA` must be the **parent** of the projects directory mounted above
(appx derives `$APPX_DATA/projects`); it defaults to `./data`.

The sslip.io host matters: subdomain routing and session cookies have to work
across project subdomains, and plain `localhost` has inconsistent
cookie-sharing behaviour for subdomains across browsers.

- Dashboard: `http://127.0.0.1.sslip.io:8080`
- Project subdomains: `http://<project>.127.0.0.1.sslip.io:8080`

For any change: edit → `task local` (Ctrl-C the running process first). There is
no hot-reload dev server — appx embeds the compiled frontend at build time, so
the local dev setup is identical to what runs on the server.

[sslip.io](https://sslip.io) is public DNS — `anything.127.0.0.1.sslip.io`
resolves to `127.0.0.1` with no setup required.

## Testing an unreleased agent-client change

To try an agent-client change against appx before it ships, override the npm
resolution rather than editing `web/package.json`:

```bash
# in the appx-agent checkout
npm run build -w packages/agent-client
cd packages/agent-client && npm link

# in appx
cd web && npm link @appx-org/agent-client
task build --force   # --force: the linked path is outside task's source globs
```

Undo with `cd web && npm unlink @appx-org/agent-client && npm install`. This is a
local-only state: land the change in appx-agent, then bump the version range in
`web/package.json` before deploying.

## Common tasks

```bash
task local              # Build and run appx in HTTP dev mode (127.0.0.1.sslip.io)
task test               # Run all Go tests
task lint               # Lint the frontend
task server:bootstrap   # First-time server setup (production, container mode)
task server:deploy      # Pull, build, install, restart (production)
task server:verify      # Post-deploy verification (production)
```

See [CLAUDE.md](../../CLAUDE.md) for architecture details and development
conventions.
