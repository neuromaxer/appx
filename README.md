# Appx

Agentic Application Proxy — a self-hostable tool to build and host apps with AI agents powered by [Pi](https://pi.dev).

![Appx dashboard](docs/readme-screenshot.png)

## What it does

Appx is a management shell for running coding agents on a remote server. It provides authentication, TLS termination, a web dashboard, and a reverse proxy — so you can manage projects, chat with agents, and access agent-built apps from a browser over HTTPS.

Appx builds on the agent stack published from **[appx-agent](https://github.com/appx-org/appx-agent)**, consumed as released artifacts — there are no sibling checkouts to clone:

| Dependency | What it is | How appx consumes it |
| --- | --- | --- |
| `agent-server` | The HTTP/SSE agent runtime wrapping Pi. Owns project identity, on-disk project directories, session transcripts, models, and credentials. | `ghcr.io/appx-org/agent-server` docker image, pulled and supervised by appx |
| [`@appx-org/agent-client`](https://www.npmjs.com/package/@appx-org/agent-client) | The TypeScript SDK and React chat UI for the agent-server `/v1` contract. | npm dependency of `web/`, pointed at the same-origin `/api/pi` mirror |
| [`@appx-org/agent-protocol`](https://www.npmjs.com/package/@appx-org/agent-protocol) | The published wire contract (OpenAPI + SSE event schema + generated types). | transitive, via agent-client |

## Architecture

```
Browser
  └── HTTPS (single port)
        ├── /              React SPA (embedded in binary)
        ├── /api/*         REST API (auth, projects, settings)
        ├── /api/pi/*       → agent-server /v1 mirror (agent-client SDK; project-scoped sessions + models)
        ├── /api/agent/*    → Pi agent-server shared auth/model proxy
        └── <project>.<domain>   Reverse proxy → agent-built apps
```

Appx itself is a single Go binary; the React frontend is compiled and embedded at build time, and state lives in a SQLite database on disk.

Pi is the agent runtime. In production appx runs as the `appx` systemd service and supervises an **outer container** built from the published `agent-server` image (agent-server + Pi + rootless podman); agent-server (published on loopback `127.0.0.1:4001`) owns project identity, directories, and sessions while sharing one set of Pi credentials, and Appx proxies session traffic to it. In local dev you run that same image by hand and appx points at it via `APPX_AGENT_SERVER_URL` (no systemd, no supervision).

**Auth model**: single user, password login, session cookie. On first run a random password is generated and written to `{data-dir}/.appx-internals/initial_password`.

**TLS**: self-signed ECDSA P-256 certificate auto-generated on first run, auto-renewed 7 days before expiry. For production, use `APPX_DOMAIN` + `CLOUDFLARE_API_TOKEN` for Let's Encrypt.

## Documentation

- **[Self-Hosting](docs/readme/self-hosting.md)** — prerequisites, the from-scratch install, provider secrets, updating, verification, troubleshooting, and known gotchas (incl. Amazon Bedrock).
- **[Networking & TLS](docs/readme/networking-and-tls.md)** — subdomain routing via sslip.io and automatic Let's Encrypt certificates.
- **[Storage & Isolation](docs/readme/storage-and-isolation.md)** — where state lives (host data dir + Docker volumes), what survives a container restart, the user/isolation model, and caveats.
- **[Local Development](docs/readme/local-development.md)** — the no-systemd dev flow (run the agent-server image by hand + `appx --http`).
- **[CLAUDE.md](CLAUDE.md)** — architecture details and development conventions.

## Prerequisites (production)

An **amd64** Linux host (Ubuntu 24.04 LTS recommended — the published agent-server image is amd64-only), `git`, and **rootful Docker**. No sibling checkouts: bootstrap pulls the agent-server image and installs the npm packages for you.

```bash
git clone https://github.com/neuromaxer/appx.git /srv/appx
cd /srv/appx
sudo ./deploy/bootstrap.sh
```

See **[Self-Hosting](docs/readme/self-hosting.md)** for the complete, ordered steps.
