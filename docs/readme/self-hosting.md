# Self-Hosting

Deploy is **container-mode only**: appx runs as the `appx` systemd service and
creates/supervises the agent-server **outer container** (one unprivileged
container holding agent-server + rootless podman). There is no host `appx-agent`
user, no `agent-server.service`, and no host install of Pi/agent-server.

appx depends only on **published artifacts** — the `ghcr.io/appx-org/agent-server`
image and the `@appx-org/agent-client` npm package, both released from the
[appx-agent](https://github.com/appx-org/appx-agent) monorepo. There are no
sibling repos to clone.

## Prerequisites

Installed manually **before** bootstrap (bootstrap does not install these):

- **Linux host** (Ubuntu **24.04 LTS** recommended — the prod target). Must be **amd64**: the published agent-server image is amd64-only, and deploy pulls it rather than building it. Ubuntu 26.04 works with one workaround (see [Known gotchas](#known-gotchas)).
- **`git`**
- **Rootful Docker**, installed and running. The outer runtime *must* be rootful host Docker (rootless docker breaks the nested rootless-podman setup).
- **Outbound access to `ghcr.io`** (to pull the agent image) and `registry.npmjs.org` (to install the web UI's dependencies).
- **Open port 443** in the firewall / cloud security group.

Everything else (Go, Node.js 24, Task, the agent-server image, and its seccomp
profile) is installed by bootstrap.

## Initial setup

```bash
# 1. Prerequisites bootstrap does NOT install: git + rootful Docker.
sudo apt-get update
sudo apt-get install -y git docker.io
sudo systemctl enable --now docker

# 2. Clone appx and run bootstrap from inside it.
git clone https://github.com/neuromaxer/appx.git /srv/appx
cd /srv/appx
sudo ./deploy/bootstrap.sh
```

On first run, bootstrap prompts for server configuration:

```
Server hostname [138.x.x.x.sslip.io]:
Data directory [/var/lib/appx]: /mnt/vol/appx-data
Port [443]: # you must open chosen port in your server firewall
```

Press Enter to accept defaults. The hostname defaults to `<your-ip>.sslip.io`
which provides free wildcard DNS — this enables subdomain routing for
agent-built apps (e.g. `https://myapp.138.x.x.x.sslip.io`). You can also use your
own domain here. For a persistent volume, mount it first and enter the mount path
as the data directory.

The config is saved to `/etc/appx/appx.env` and reused on subsequent runs. To
change it later: `sudo nano /etc/appx/appx.env && sudo systemctl restart appx`.

Bootstrap then creates the `appx` OS user, installs the build toolchain (Go,
Node.js, Task), **pulls the pinned `ghcr.io/appx-org/agent-server` image** and
extracts its tailored seccomp profile to `/etc/appx/seccomp-builder.json`,
installs the `appx` systemd service, starts it, and runs a verification suite.
agent-server inside the container runs with `NODE_USE_ENV_PROXY=1` +
`HTTPS_PROXY` pointed at appx's egress proxy on the docker bridge gateway, so
provider traffic goes through the Appx egress allowlist.

The seccomp profile is the outer container's security boundary and is taken out
of the image rather than vendored in this repo, so the profile appx applies is
always the one the pulled image was built with.

**Provider credentials.** Configure them in the **Settings UI** after first
login — Anthropic and most providers are stored in the agent's Pi credential
storage (persisted in the `builder-workspace` volume), just like any other key.
Only credentials the Settings UI can't carry (e.g. Amazon Bedrock — an upstream
Pi gap) need the service-env path — see [Known gotchas](#known-gotchas).

After bootstrap finishes, grab the generated password and log in:

```bash
sudo cat {data directory path from bootstrap}/.appx-internals/initial_password   # delete after saving
```

Visit `https://<host>` (self-signed cert by default → browser warning; for a
trusted cert see [Networking & TLS](./networking-and-tls.md)). Open **Settings**
to configure your model-provider credentials and models, then create a project.

## Known gotchas

- **Amazon Bedrock (or any non-Anthropic provider).** The bootstrap prompt only covers `ANTHROPIC_API_KEY`. For Bedrock, after bootstrap put the creds in `secrets.env`, list the var names in `APPX_AGENT_ENV_PASSTHROUGH`, and **recreate** the outer container (passthrough vars are injected at container *create* time, so a plain restart won't pick them up):

  ```bash
  sudo tee /etc/appx/secrets.env >/dev/null <<'EOF'
  AWS_BEARER_TOKEN_BEDROCK=<your-token>
  AWS_REGION=eu-central-1
  EOF
  sudo chown root:root /etc/appx/secrets.env && sudo chmod 600 /etc/appx/secrets.env

  sudo sed -i 's/^# APPX_AGENT_ENV_PASSTHROUGH=.*/APPX_AGENT_ENV_PASSTHROUGH=AWS_BEARER_TOKEN_BEDROCK,AWS_REGION/' /etc/appx/appx.env

  sudo systemctl stop appx && docker rm -f builder-outer && sudo systemctl start appx
  ```

  `bedrock-runtime.*.amazonaws.com:443` is already in the egress allowlist. (Note: setting a Bedrock key via the Settings UI does **not** work yet — an upstream Pi gap; the env var is the supported path.)

- **Ubuntu 26.04 only:** `apt-get install -y task` fails (the cloudsmith repo has no 26.04 release), which aborts `tools-install.sh`. Pre-install Task, then re-run bootstrap (no-op on 24.04):

  ```bash
  sudo sh -c 'curl -1sLf https://taskfile.dev/install.sh | sh -s -- -d -b /usr/local/bin'
  ```

- **docker-group timing.** bootstrap adds `appx` to the `docker` group; the *service* inherits it on its next start (bootstrap handles that). A human shell needs a re-login to use docker without sudo.

- **A new image or new passthrough env needs a recreate, not just a restart.** A plain `systemctl restart appx` only re-attaches to the running container. To pick up a newly pulled agent-server image or a changed `APPX_AGENT_ENV_PASSTHROUGH`, recreate it (the `builder-workspace` + `builder-podman-storage` volumes — i.e. projects, sessions, inner images — survive):

  ```bash
  sudo systemctl stop appx && docker rm -f builder-outer && sudo systemctl start appx
  ```

## Updating appx

After pushing a new release:

```bash
cd /srv/appx
task server:deploy
```

Pulls latest code, rebuilds the appx binary, installs it, re-pulls the pinned
agent-server image (refreshing the extracted seccomp profile), and restarts
`appx` (which re-attaches to the outer container — see the recreate note above to
force a fresh image/env).

## Updating the agent (Pi / agent-server)

Pi and agent-server run **inside** the agent-server image, so updating them means
moving to a newer published image.

The version appx is tested against lives in the **`AGENT_VERSION`** file at the
repo root — one line, the single source of truth for both the docker image and the
`@appx-org/agent-client` npm package (appx-agent versions them in lockstep). To
move the whole checkout to a new agent release:

```bash
echo 0.1.8 > AGENT_VERSION
cd web && npm install && cd ..   # updates package.json + lockfile
task test                        # fails if you skip the npm install
```

Then deploy normally with `task server:deploy`.

To move an existing box to a different version **without** changing the checkout,
set the ref in `/etc/appx/appx.env` and recreate:

```bash
sudo sed -i 's|^APPX_AGENT_IMAGE=.*|APPX_AGENT_IMAGE=ghcr.io/appx-org/agent-server:X.Y.Z|' /etc/appx/appx.env
sudo ./deploy/tools-install.sh   # pulls it + re-extracts the seccomp profile
sudo systemctl stop appx && docker rm -f builder-outer && sudo systemctl start appx
```

Note the recreate is required: appx refuses to silently recreate a running
container when the image or pinned ports drift (it would kill running apps).
Alternatively pass `--recreate-agent-container` (or
`APPX_RECREATE_AGENT_CONTAINER=true`).

Available tags are listed on the
[appx-agent packages page](https://github.com/appx-org/appx-agent/pkgs/container/agent-server);
pin a semver tag or a `@sha256:` digest, not `latest` or `edge`.

## Verify installation

```bash
sudo ./deploy/verify-installation.sh
```

Container-mode checks: the `appx` unit is active and ordered after docker, the
outer container is healthy with the proven security flags + loopback-only
publishes + `RestartPolicy=unless-stopped`, the pulled image ships the seccomp
profile and the installed copy matches it, no secret leaked into the journal, and
no host-mode artifacts remain (if a provider cred is supplied via the env path,
it also checks that it's reachable inside the container). Exits 0 only if
everything passes.

## Troubleshoot

```bash
journalctl -u appx -f            # appx logs (incl. container supervision)
docker logs -f builder-outer     # agent-server (inside the outer container)
```

## Deploy scripts

| File / Script                   | When             | What                                                       |
| ------------------------------- | ---------------- | ---------------------------------------------------------- |
| `deploy/bootstrap.sh`           | Day 1            | Full setup: user, dirs, tools, agent image, build, start, verify |
| `deploy/system-setup.sh`        | Infra changes    | appx user, projects group, dirs, `/etc/appx`, docker group, unit |
| `deploy/tools-install.sh`       | Tool updates     | Go, Node.js 24, Task, + pulls the agent image and extracts its seccomp profile |
| `deploy/agent-version.sh`       | sourced, not run | Resolves `AGENT_VERSION` → `$AGENT_IMAGE` for the other scripts |
| `deploy/appx.service`           | systemd unit     | `appx` unit (container mode; ordered after `docker.service`) |
| `deploy/verify-installation.sh` | After any change | Full system verification                                   |
| `deploy/teardown.sh`            | Uninstall & cleanup | Reverse everything created by bootstrap.sh                        |
