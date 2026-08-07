package containerruntime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Config is the appx-side configuration used to build a ContainerSpec. It is
// populated from env/flags in main.go and kept separate from ContainerSpec so
// spec construction is a pure, unit-testable function.
type Config struct {
	Image              string // APPX_AGENT_IMAGE (default "builder-outer")
	Name               string // APPX_AGENT_CONTAINER_NAME (default "builder-outer")
	SeccompProfilePath string // APPX_AGENT_SECCOMP (required)

	APIBindHost string // loopback host for the API publish (127.0.0.1)
	APIPort     int    // agent-server API port (4001)

	AppBindHost  string // loopback host for the app range publish (127.0.0.1)
	AppPortStart int    // project.PortRangeStart (10000)
	AppPortEnd   int    // project.PublishedPortRangeEnd (10199)

	WorkspaceVolume string // named volume → /workspace
	PodmanVolume    string // named volume → ~/.local/share/containers

	// Token is the AGENT_SERVER_TOKEN injected into the container (mandatory in
	// container mode).
	Token string
	// EnvPassthrough is the list of env var names forwarded by name (secrets).
	EnvPassthrough []string

	// HostGateway is the host alias value for host.docker.internal (usually
	// "host-gateway"). The container reaches appx's egress proxy via this.
	HostGateway string
	// EgressProxyURL is the HTTPS_PROXY value the container uses to reach appx's
	// CONNECT proxy (e.g. "http://host.docker.internal:9080"). Empty disables
	// proxy env injection.
	EgressProxyURL string
	// NoProxy is the NO_PROXY value (keeps in-container loopback direct).
	NoProxy string

	// Memory and CPUs are the outer container's resource ceiling (--memory,
	// --cpus). Empty defaults to DefaultMemory/DefaultCPUs; set to "unlimited"
	// to omit the flag entirely and let the container use the whole host.
	Memory string
	CPUs   string

	ReadinessURL string // agent-server health URL (e.g. http://127.0.0.1:4001/)

	// RestartPolicy is the docker --restart policy. Empty defaults to
	// DefaultRestartPolicy ("unless-stopped") so the daemon keeps the outer
	// container alive across crashes + reboots independent of appx.
	RestartPolicy string
}

// defaults for the named volumes and bind hosts — match the proven run-outer.sh.
const (
	// DefaultImage is the published agent-server image. appx consumes the
	// artifact released from the appx-org/appx-agent monorepo and never builds it
	// from source; the tag is a semver release, pinned here so an appx build
	// always knows which agent contract it was tested against. Override with
	// APPX_AGENT_IMAGE (a tag or a @sha256: digest).
	DefaultImage = "ghcr.io/appx-org/agent-server:0.1.6"
	// DefaultName is the local docker container name — unrelated to the image
	// ref, and kept stable so existing deployments' troubleshooting commands
	// (docker logs builder-outer) and volumes keep working.
	DefaultName            = "builder-outer"
	DefaultWorkspaceVolume = "builder-workspace"
	DefaultPodmanVolume    = "builder-podman-storage"
	DefaultPodmanDest      = "/home/builder/.local/share/containers"
	DefaultWorkspaceDest   = "/workspace"
	DefaultBindHost        = "127.0.0.1"
	// DefaultRestartPolicy makes the Docker daemon resurrect the outer container
	// on crash and on reboot (Stage 4 supervision model): the daemon keeps the
	// container process alive; appx ensures it exists/is-correct/is-healthy at
	// startup; appx.service Restart=on-failure covers appx itself.
	DefaultRestartPolicy = "unless-stopped"

	// DefaultMemory and DefaultCPUs bound the outer container so an LLM-driven
	// `npm install` or image build cannot exhaust the host. The agent runs
	// untrusted, model-authored build steps, so "no limit" is not a safe
	// default — especially once agent-server shares a host with other services
	// (see openorange's Apps integration). These are conservative starting
	// values, not measurements: Node plus nested podman builds need real
	// headroom. Raise them via APPX_AGENT_MEMORY / APPX_AGENT_CPUS if builds are
	// SIGKILLed near the ceiling.
	DefaultMemory = "4g"
	DefaultCPUs   = "2.0"

	// UnlimitedResources opts out of the ceiling for a single field, restoring
	// the pre-default behaviour of omitting the docker flag.
	UnlimitedResources = "unlimited"
)

// BuildSpec turns a Config into a ContainerSpec, applying defaults. It is pure
// (no I/O) so it can be unit-tested for the verbatim flag set.
func BuildSpec(cfg Config) ContainerSpec {
	if cfg.Image == "" {
		cfg.Image = DefaultImage
	}
	if cfg.Name == "" {
		cfg.Name = DefaultName
	}
	if cfg.APIBindHost == "" {
		cfg.APIBindHost = DefaultBindHost
	}
	if cfg.AppBindHost == "" {
		cfg.AppBindHost = DefaultBindHost
	}
	if cfg.WorkspaceVolume == "" {
		cfg.WorkspaceVolume = DefaultWorkspaceVolume
	}
	if cfg.PodmanVolume == "" {
		cfg.PodmanVolume = DefaultPodmanVolume
	}
	if cfg.RestartPolicy == "" {
		cfg.RestartPolicy = DefaultRestartPolicy
	}
	if cfg.Memory == "" {
		cfg.Memory = DefaultMemory
	}
	if cfg.CPUs == "" {
		cfg.CPUs = DefaultCPUs
	}
	// "unlimited" is the explicit opt-out: clear the field so RunArgs omits the
	// flag, which is what an empty value used to mean before these defaults.
	if cfg.Memory == UnlimitedResources {
		cfg.Memory = ""
	}
	if cfg.CPUs == UnlimitedResources {
		cfg.CPUs = ""
	}

	env := map[string]string{
		// WORKSPACE_DIR is baked into the image too, but appx sets it explicitly
		// so the contract is visible in `docker inspect` and survives an image
		// that forgets it.
		"WORKSPACE_DIR": DefaultWorkspaceDest,
	}
	if cfg.Token != "" {
		env["AGENT_SERVER_TOKEN"] = cfg.Token
	}
	// Egress: route agent-server's provider HTTPS through appx's CONNECT proxy on
	// the bridge gateway. Mirrors deploy/agent-server.service, but the proxy is
	// reached via host.docker.internal (loopback no longer crosses the boundary).
	if cfg.EgressProxyURL != "" {
		env["HTTPS_PROXY"] = cfg.EgressProxyURL
		env["NODE_USE_ENV_PROXY"] = "1"
		if cfg.NoProxy != "" {
			env["NO_PROXY"] = cfg.NoProxy
		}
	}

	var addHosts []string
	if cfg.HostGateway != "" {
		addHosts = append(addHosts, "host.docker.internal:"+cfg.HostGateway)
	}

	return ContainerSpec{
		Image:              cfg.Image,
		Name:               cfg.Name,
		SeccompProfilePath: cfg.SeccompProfilePath,
		APIBindHost:        cfg.APIBindHost,
		APIPort:            cfg.APIPort,
		AppBindHost:        cfg.AppBindHost,
		AppPortStart:       cfg.AppPortStart,
		AppPortEnd:         cfg.AppPortEnd,
		Volumes: []VolumeMount{
			{Name: cfg.WorkspaceVolume, Dest: DefaultWorkspaceDest},
			{Name: cfg.PodmanVolume, Dest: DefaultPodmanDest},
		},
		Env:            env,
		EnvPassthrough: cfg.EnvPassthrough,
		AddHosts:       addHosts,
		Memory:         cfg.Memory,
		CPUs:           cfg.CPUs,
		ReadinessURL:   cfg.ReadinessURL,
		RestartPolicy:  cfg.RestartPolicy,
	}
}

// LoadOrCreateToken returns the bearer token persisted at path, generating and
// writing a fresh 32-byte hex token (0600) on first use. The token authenticates
// appx → agent-server now that the API port is published (loopback is no longer
// a sufficient trust boundary on a multi-process host). It is NEVER committed.
func LoadOrCreateToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		tok := strings.TrimSpace(string(data))
		if tok != "" {
			// Tighten perms in case the file pre-existed looser than 0600.
			_ = os.Chmod(path, 0600)
			return tok, nil
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("read agent token %s: %w", path, err)
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate agent token: %w", err)
	}
	tok := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(tok+"\n"), 0600); err != nil {
		return "", fmt.Errorf("persist agent token %s: %w", path, err)
	}
	// Enforce 0600 even if the file pre-existed with looser perms.
	_ = os.Chmod(path, 0600)
	return tok, nil
}

// DetectBin returns the container CLI to drive: "docker" if present, else
// "podman", else "docker" (so the daemon-unavailable error path fires with a
// sensible binary name). Allows an explicit override via the bin argument.
func DetectBin(override string, lookPath func(string) (string, error)) string {
	if override != "" {
		return override
	}
	for _, cand := range []string{"docker", "podman"} {
		if _, err := lookPath(cand); err == nil {
			return cand
		}
	}
	return "docker"
}

// BridgeGateway returns the default-bridge gateway IP the container sees as
// host.docker.internal (host-gateway). appx binds its egress proxy here so the
// container can reach it. Uses `docker network inspect bridge`.
func BridgeGateway(ctx context.Context, bin string, runner CommandRunner) (string, error) {
	stdout, stderr, err := runner.Run(ctx, bin, "network", "inspect", "bridge",
		"--format", "{{range .IPAM.Config}}{{.Gateway}}{{end}}")
	if err != nil {
		return "", fmt.Errorf("%s network inspect bridge: %v: %s", bin, err, strings.TrimSpace(string(stderr)))
	}
	gw := strings.TrimSpace(string(stdout))
	if gw == "" {
		// Fall back to parsing the JSON form in case the template yielded nothing
		// (e.g. podman's differing schema).
		gw = parseGatewayJSON(ctx, bin, runner)
	}
	if gw == "" {
		return "", fmt.Errorf("could not determine bridge gateway from %s network inspect", bin)
	}
	return gw, nil
}

func parseGatewayJSON(ctx context.Context, bin string, runner CommandRunner) string {
	stdout, _, err := runner.Run(ctx, bin, "network", "inspect", "bridge")
	if err != nil {
		return ""
	}
	var nets []struct {
		IPAM struct {
			Config []struct {
				Gateway string `json:"Gateway"`
			} `json:"Config"`
		} `json:"IPAM"`
		// podman uses lowercase "subnets" with "gateway"
		Subnets []struct {
			Gateway string `json:"gateway"`
		} `json:"subnets"`
	}
	if err := json.Unmarshal(stdout, &nets); err != nil {
		return ""
	}
	for _, n := range nets {
		for _, c := range n.IPAM.Config {
			if c.Gateway != "" {
				return c.Gateway
			}
		}
		for _, s := range n.Subnets {
			if s.Gateway != "" {
				return s.Gateway
			}
		}
	}
	return ""
}
