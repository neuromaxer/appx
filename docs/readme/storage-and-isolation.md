# Storage & Isolation

## Persistent storage

State lives in two places: appx's own data on the host, and the agent +
app data inside Docker volumes.

**On the host** — the data directory (configured during bootstrap, default
`/var/lib/appx`):

| Contents                                            | Path                      | Access     |
| --------------------------------------------------- | ------------------------- | ---------- |
| SQLite DB, TLS certs, initial password, agent token | `{data}/.appx-internals/` | `appx` only (0700) |

Provider credentials set via the **Settings UI** are stored in the agent's Pi
credential storage **inside the container** (persisted in the `builder-workspace`
volume), not in the host data directory. The exception is env-only credentials
(e.g. Amazon Bedrock), supplied via the service environment
(`/etc/appx/secrets.env`, `root:root 0600`) and forwarded into the container by
name — never written to the data directory.

**Inside Docker volumes** — created and owned by the daemon, mounted into the
outer container:

| Contents                                  | Volume                    | Mount |
| ----------------------------------------- | ------------------------- | ----- |
| Project workspaces + Pi sessions          | `builder-workspace`       | `/workspace` |
| Inner app images + containers (podman)     | `builder-podman-storage`  | `~/.local/share/containers` |

These volumes are **independent of the container's lifecycle**: they survive a
container crash, `docker rm -f`, recreate, and host reboot. Only an explicit
`docker volume rm` destroys them — never do that in production. This is what lets
the outer container be safely recreated (to pick up a new image or env) without
losing projects or sessions.

> Local co-located dev is different: there you bind-mount `{data}/projects/` into
> the agent-server container, so projects land on the host (see
> [Local Development](./local-development.md)). In production (container mode)
> projects live in the `builder-workspace` volume, not on the host.

To use a mounted volume for the host data directory, specify the path when
bootstrap prompts for "Data directory"; bootstrap creates the subdirectories with
correct permissions.

## Credentials & the Agent tab

Pi credentials are configured from Settings. Built-in providers can use stored
API keys or Pi subscription auth where the provider supports it; custom providers
(e.g. LiteLLM) are written to the agent's Pi storage inside the container without
exposing secret values back to the browser.

The Agent tab is the `@appx-org/agent-client` SDK talking to Appx's same-origin
`/api/pi/*` mirror, which proxies the `agent-server` `/v1` session contract
(keeping the bearer token server-side). `agent-server` turns all supported Pi
providers into the same HTTP/SSE session contract. Pi extension UI requests —
including Appx guardrail approvals for risky commands — are delivered over the
same session stream and answered through the mirror.

## User isolation

Container mode has a **single host user, `appx`**, which runs the appx server and
owns its DB, TLS certs, and binary. The agent (agent-server + Pi) and **all
project workspaces run inside the outer container as an unprivileged uid (1000)**,
not as a host user — so the container, not host-user separation, is the isolation
boundary between agent workloads and the host. (The legacy host `appx-agent` user
and the shared `projects` group from the old host-mode deploy are gone.)

Two residual-risk notes, both accepted for a dedicated single-purpose box:

- **`appx` is in the `docker` group** to drive the daemon, which is
  **root-equivalent** (`docker run -v /:/host` owns the box). Scoping it down (a
  docker-socket proxy or a narrow sudoers rule) is tracked as hardening.
- **The outer container's security boundary is the load-bearing isolation**:
  unprivileged (`Privileged=false`, no added caps, no `/dev/fuse`), a tailored
  seccomp profile (extracted from the agent-server image at install time), and
  loopback-only port publishes. See
  [`packages/agent-server/container/SPIKE-FINDINGS.md`](https://github.com/appx-org/appx-agent/blob/main/packages/agent-server/container/SPIKE-FINDINGS.md)
  in appx-agent for the full justification.

## Caveats

- **Self-signed TLS (default).** Browsers show a security warning. Set `APPX_DOMAIN` + `CLOUDFLARE_API_TOKEN` for automatic Let's Encrypt (see [Networking & TLS](./networking-and-tls.md)).
- **Single-user only.** One password, one session store. Designed for personal use.
- **Port 443 binding.** Handled without root via `CAP_NET_BIND_SERVICE` on the systemd unit; for hand-running the binary use `-port 8443` or grant the cap manually.
