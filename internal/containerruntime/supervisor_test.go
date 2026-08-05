package containerruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// scriptedCall is one recorded/expected CLI invocation for fakeRunner.
type scriptedCall struct {
	stdout string
	stderr string
	err    error
}

// fakeRunner is a scripted CommandRunner. It matches the first verb of each call
// (inspect/run/start/rm/network) to a queue of responses and records every
// invocation's full argv for assertions.
type fakeRunner struct {
	responses map[string][]scriptedCall
	calls     [][]string
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{responses: map[string][]scriptedCall{}}
}

func (f *fakeRunner) push(verb string, c scriptedCall) {
	f.responses[verb] = append(f.responses[verb], c)
}

func (f *fakeRunner) Run(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
	f.calls = append(f.calls, args)
	verb := ""
	if len(args) > 0 {
		verb = args[0]
	}
	queue := f.responses[verb]
	if len(queue) == 0 {
		return nil, nil, fmt.Errorf("fakeRunner: no scripted response for verb %q (argv=%v)", verb, args)
	}
	c := queue[0]
	f.responses[verb] = queue[1:]
	return []byte(c.stdout), []byte(c.stderr), c.err
}

func (f *fakeRunner) countVerb(verb string) int {
	n := 0
	for _, c := range f.calls {
		if len(c) > 0 && c[0] == verb {
			n++
		}
	}
	return n
}

func testSpec() ContainerSpec {
	return BuildSpec(Config{
		Image:              "builder-outer",
		Name:               "builder-outer",
		SeccompProfilePath: "/etc/appx/seccomp-builder.json",
		APIPort:            4001,
		AppPortStart:       10000,
		AppPortEnd:         10199,
		Token:              "tok123",
		EnvPassthrough:     []string{"ANTHROPIC_API_KEY"},
		HostGateway:        "host-gateway",
		EgressProxyURL:     "http://host.docker.internal:9080",
		NoProxy:            "localhost,127.0.0.1",
		ReadinessURL:       "http://127.0.0.1:4001/",
	})
}

func okPing(context.Context, string) error { return nil }

func newTestSupervisor(r CommandRunner, ping func(context.Context, string) error) *DockerSupervisor {
	return NewDockerSupervisor("docker",
		WithRunner(r), WithPing(ping),
		WithReadyTimeout(50*time.Millisecond), WithPollInterval(time.Millisecond))
}

// --- RunArgs: the verbatim security flag set ---------------------------------

func TestRunArgs_VerbatimSecurityFlagSet(t *testing.T) {
	args := testSpec().RunArgs()
	joined := strings.Join(args, " ")

	// Required proven flags.
	mustContain := []string{
		"--device /dev/net/tun",
		"--security-opt seccomp=/etc/appx/seccomp-builder.json",
		"--security-opt apparmor=unconfined",
		"--security-opt systempaths=unconfined",
		"-v builder-workspace:/workspace",
		"-v builder-podman-storage:/home/builder/.local/share/containers",
		"-p 127.0.0.1:4001:4001",
		"-p 127.0.0.1:10000-10199:10000-10199",
		"--add-host host.docker.internal:host-gateway",
		"-e ANTHROPIC_API_KEY",
		"-e AGENT_SERVER_TOKEN=tok123",
		"-e WORKSPACE_DIR=/workspace",
		"-e HTTPS_PROXY=http://host.docker.internal:9080",
		"-e NODE_USE_ENV_PROXY=1",
		"-e NO_PROXY=localhost,127.0.0.1",
		"--restart unless-stopped",
	}
	for _, want := range mustContain {
		if !strings.Contains(joined, want) {
			t.Errorf("RunArgs missing %q\n got: %s", want, joined)
		}
	}

	// Forbidden flags — the security boundary.
	mustNotContain := []string{
		"--privileged",
		"--cap-add",
		"/dev/fuse",
		"seccomp=unconfined",
		"no-new-privileges",
		"--network=host",
		"--network host",
		"0.0.0.0:4001",
		"0.0.0.0:10000",
	}
	for _, bad := range mustNotContain {
		if strings.Contains(joined, bad) {
			t.Errorf("RunArgs must NOT contain %q\n got: %s", bad, joined)
		}
	}

	// Image must be the final argument.
	if args[len(args)-1] != "builder-outer" {
		t.Errorf("image must be the last arg, got %q", args[len(args)-1])
	}
}

// TestRunArgs_ResourceLimits covers the outer container's resource ceiling: it
// is ON by default (an LLM-driven build must not be able to exhaust the host),
// overridable, and explicitly opt-out-able via "unlimited".
func TestRunArgs_ResourceLimits(t *testing.T) {
	// Default: the ceiling is applied without the operator asking for it.
	joined := strings.Join(testSpec().RunArgs(), " ")
	if !strings.Contains(joined, "--memory "+DefaultMemory) {
		t.Errorf("default spec should include --memory %s, got %s", DefaultMemory, joined)
	}
	if !strings.Contains(joined, "--cpus "+DefaultCPUs) {
		t.Errorf("default spec should include --cpus %s, got %s", DefaultCPUs, joined)
	}

	// Explicit values override the defaults.
	spec := testSpec()
	spec.Memory = "2g"
	spec.CPUs = "1.5"
	joined = strings.Join(spec.RunArgs(), " ")
	if !strings.Contains(joined, "--memory 2g") {
		t.Errorf("expected --memory 2g, got %s", joined)
	}
	if !strings.Contains(joined, "--cpus 1.5") {
		t.Errorf("expected --cpus 1.5, got %s", joined)
	}

	// "unlimited" omits the flag entirely, restoring the pre-default behaviour.
	unlimited := BuildSpec(Config{
		Image:              "builder-outer",
		Name:               "builder-outer",
		SeccompProfilePath: "/etc/appx/seccomp-builder.json",
		APIPort:            4001,
		AppPortStart:       10000,
		AppPortEnd:         10199,
		Memory:             UnlimitedResources,
		CPUs:               UnlimitedResources,
	})
	joined = strings.Join(unlimited.RunArgs(), " ")
	if strings.Contains(joined, "--memory") {
		t.Errorf("Memory=%q should omit --memory, got %s", UnlimitedResources, joined)
	}
	if strings.Contains(joined, "--cpus") {
		t.Errorf("CPUs=%q should omit --cpus, got %s", UnlimitedResources, joined)
	}
}

// TestRunArgs_RestartPolicy asserts the daemon-driven restart policy is in the
// arg vector (Stage 4: the daemon keeps the outer container alive across crash +
// reboot, independent of appx). Default is unless-stopped; empty emits nothing.
func TestRunArgs_RestartPolicy(t *testing.T) {
	if !strings.Contains(strings.Join(testSpec().RunArgs(), " "), "--restart unless-stopped") {
		t.Error("default spec should include --restart unless-stopped")
	}
	spec := testSpec()
	spec.RestartPolicy = ""
	if strings.Contains(strings.Join(spec.RunArgs(), " "), "--restart") {
		t.Error("empty RestartPolicy must not emit --restart")
	}
}

func TestSpec_ValidateMissingFields(t *testing.T) {
	if err := (ContainerSpec{}).Validate(); err == nil {
		t.Fatal("expected validation error for empty spec")
	}
	if err := testSpec().Validate(); err != nil {
		t.Errorf("valid spec failed validation: %v", err)
	}
}

// --- EnsureRunning state machine --------------------------------------------

func TestEnsureRunning_AbsentCreates(t *testing.T) {
	r := newFakeRunner()
	r.push("inspect", scriptedCall{stderr: "Error: No such object: builder-outer", err: errors.New("exit 1")})
	r.push("run", scriptedCall{stdout: "containerid\n"})

	sup := newTestSupervisor(r, okPing)
	if err := sup.EnsureRunning(context.Background(), testSpec()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if r.countVerb("run") != 1 {
		t.Errorf("expected 1 run, got %d", r.countVerb("run"))
	}
	if r.countVerb("start") != 0 {
		t.Errorf("expected 0 start, got %d", r.countVerb("start"))
	}
}

func TestEnsureRunning_StoppedStarts(t *testing.T) {
	r := newFakeRunner()
	r.push("inspect", scriptedCall{stdout: inspectJSON("exited", false, "builder-outer")})
	r.push("start", scriptedCall{stdout: "builder-outer\n"})

	sup := newTestSupervisor(r, okPing)
	if err := sup.EnsureRunning(context.Background(), testSpec()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if r.countVerb("start") != 1 {
		t.Errorf("expected 1 start, got %d", r.countVerb("start"))
	}
	if r.countVerb("run") != 0 {
		t.Errorf("expected 0 run, got %d", r.countVerb("run"))
	}
}

func TestEnsureRunning_RunningNoop(t *testing.T) {
	r := newFakeRunner()
	r.push("inspect", scriptedCall{stdout: inspectJSON("running", true, "builder-outer")})

	sup := newTestSupervisor(r, okPing)
	if err := sup.EnsureRunning(context.Background(), testSpec()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if r.countVerb("run") != 0 || r.countVerb("start") != 0 {
		t.Errorf("running container should be a noop, got run=%d start=%d",
			r.countVerb("run"), r.countVerb("start"))
	}
}

func TestEnsureRunning_UnhealthyTimesOut(t *testing.T) {
	r := newFakeRunner()
	r.push("inspect", scriptedCall{stdout: inspectJSON("running", true, "builder-outer")})

	sup := newTestSupervisor(r, func(context.Context, string) error {
		return errors.New("connection refused")
	})
	err := sup.EnsureRunning(context.Background(), testSpec())
	if !errors.Is(err, ErrUnhealthy) {
		t.Fatalf("expected ErrUnhealthy, got %v", err)
	}
}

func TestEnsureRunning_DaemonUnavailable(t *testing.T) {
	r := newFakeRunner()
	r.push("inspect", scriptedCall{
		stderr: "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?",
		err:    errors.New("exit 1"),
	})
	sup := newTestSupervisor(r, okPing)
	err := sup.EnsureRunning(context.Background(), testSpec())
	if !errors.Is(err, ErrDaemonUnavailable) {
		t.Fatalf("expected ErrDaemonUnavailable, got %v", err)
	}
}

func TestEnsureRunning_ImageMissing(t *testing.T) {
	r := newFakeRunner()
	r.push("inspect", scriptedCall{stderr: "Error: No such object: builder-outer", err: errors.New("exit 1")})
	r.push("run", scriptedCall{
		stderr: "Unable to find image 'builder-outer:latest' locally\ndocker: Error response from daemon: pull access denied",
		err:    errors.New("exit 125"),
	})
	sup := newTestSupervisor(r, okPing)
	err := sup.EnsureRunning(context.Background(), testSpec())
	if !errors.Is(err, ErrImageMissing) {
		t.Fatalf("expected ErrImageMissing, got %v", err)
	}
}

func TestEnsureRunning_SpecDriftDoesNotRecreate(t *testing.T) {
	r := newFakeRunner()
	// Running, but created from a different image tag.
	r.push("inspect", scriptedCall{stdout: inspectJSON("running", true, "builder-outer:old")})

	sup := newTestSupervisor(r, okPing)
	err := sup.EnsureRunning(context.Background(), testSpec())
	var drift *SpecDriftError
	if !errors.As(err, &drift) {
		t.Fatalf("expected *SpecDriftError, got %v", err)
	}
	if r.countVerb("run") != 0 || r.countVerb("rm") != 0 {
		t.Error("drift must NOT recreate the container")
	}
	if !strings.Contains(drift.Error(), "--recreate-agent-container") {
		t.Errorf("drift error should mention remediation flag, got %q", drift.Error())
	}
}

func TestEnsureRunning_DriftMissingPublish(t *testing.T) {
	r := newFakeRunner()
	// Right image, but the app range isn't published.
	r.push("inspect", scriptedCall{stdout: `[{"State":{"Status":"running","Running":true},"Config":{"Image":"builder-outer"},"Image":"sha256:abc","HostConfig":{"PortBindings":{"4001/tcp":[{"HostIp":"127.0.0.1","HostPort":"4001"}]}}}]`})
	sup := newTestSupervisor(r, okPing)
	err := sup.EnsureRunning(context.Background(), testSpec())
	var drift *SpecDriftError
	if !errors.As(err, &drift) {
		t.Fatalf("expected *SpecDriftError, got %v", err)
	}
}

func TestRecreate_ForceRemovesThenCreates(t *testing.T) {
	r := newFakeRunner()
	r.push("rm", scriptedCall{stdout: "builder-outer\n"})
	r.push("run", scriptedCall{stdout: "containerid\n"})

	sup := newTestSupervisor(r, okPing)
	if err := sup.Recreate(context.Background(), testSpec()); err != nil {
		t.Fatalf("Recreate: %v", err)
	}
	if r.countVerb("rm") != 1 || r.countVerb("run") != 1 {
		t.Errorf("expected rm+run, got rm=%d run=%d", r.countVerb("rm"), r.countVerb("run"))
	}
}

func TestRecreate_IgnoresAbsentContainer(t *testing.T) {
	r := newFakeRunner()
	r.push("rm", scriptedCall{stderr: "Error: No such object: builder-outer", err: errors.New("exit 1")})
	r.push("run", scriptedCall{stdout: "containerid\n"})
	sup := newTestSupervisor(r, okPing)
	if err := sup.Recreate(context.Background(), testSpec()); err != nil {
		t.Fatalf("Recreate should ignore absent container: %v", err)
	}
}

func TestStatus_Absent(t *testing.T) {
	r := newFakeRunner()
	r.push("inspect", scriptedCall{stderr: "Error: No such object: x", err: errors.New("exit 1")})
	sup := newTestSupervisor(r, okPing)
	st, err := sup.Status(context.Background(), "x")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Exists {
		t.Error("expected Exists=false")
	}
}

// --- token persistence -------------------------------------------------------

func TestLoadOrCreateToken_GeneratesAndPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-server-token")

	tok1, err := LoadOrCreateToken(path)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if len(tok1) < 32 {
		t.Errorf("token too short: %q", tok1)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("expected 0600 perms, got %o", perm)
	}

	tok2, err := LoadOrCreateToken(path)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if tok1 != tok2 {
		t.Errorf("token not stable across loads: %q vs %q", tok1, tok2)
	}
}

func TestLoadOrCreateToken_TightensPerms(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tok")
	if err := os.WriteFile(path, []byte("preexisting\n"), 0644); err != nil {
		t.Fatal(err)
	}
	tok, err := LoadOrCreateToken(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if tok != "preexisting" {
		t.Errorf("expected existing token reused, got %q", tok)
	}
	info, _ := os.Stat(path)
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("expected perms tightened to 0600, got %o", perm)
	}
}

// --- spec construction from config ------------------------------------------

func TestBuildSpec_Defaults(t *testing.T) {
	spec := BuildSpec(Config{
		SeccompProfilePath: "/p",
		APIPort:            4001,
		AppPortStart:       10000,
		AppPortEnd:         10199,
		ReadinessURL:       "http://127.0.0.1:4001/",
	})
	if spec.Image != DefaultImage || spec.Name != DefaultName {
		t.Errorf("defaults not applied: image=%q name=%q", spec.Image, spec.Name)
	}
	if spec.APIBindHost != "127.0.0.1" || spec.AppBindHost != "127.0.0.1" {
		t.Error("bind hosts must default to loopback")
	}
	if len(spec.Volumes) != 2 {
		t.Fatalf("expected 2 volumes, got %d", len(spec.Volumes))
	}
	if spec.Env["WORKSPACE_DIR"] != "/workspace" {
		t.Errorf("WORKSPACE_DIR not set: %v", spec.Env)
	}
	// No proxy URL → no proxy env injected.
	if _, ok := spec.Env["HTTPS_PROXY"]; ok {
		t.Error("HTTPS_PROXY should be absent when EgressProxyURL is empty")
	}
}

func TestDetectBin(t *testing.T) {
	// override wins.
	if got := DetectBin("podman", func(string) (string, error) { return "", errors.New("nope") }); got != "podman" {
		t.Errorf("override ignored: %q", got)
	}
	// docker found.
	if got := DetectBin("", func(s string) (string, error) {
		if s == "docker" {
			return "/usr/bin/docker", nil
		}
		return "", errors.New("nope")
	}); got != "docker" {
		t.Errorf("expected docker, got %q", got)
	}
	// only podman found.
	if got := DetectBin("", func(s string) (string, error) {
		if s == "podman" {
			return "/usr/bin/podman", nil
		}
		return "", errors.New("nope")
	}); got != "podman" {
		t.Errorf("expected podman, got %q", got)
	}
}

func TestBridgeGateway(t *testing.T) {
	r := newFakeRunner()
	r.push("network", scriptedCall{stdout: "172.17.0.1\n"})
	gw, err := BridgeGateway(context.Background(), "docker", r)
	if err != nil {
		t.Fatalf("BridgeGateway: %v", err)
	}
	if gw != "172.17.0.1" {
		t.Errorf("expected 172.17.0.1, got %q", gw)
	}
}

// inspectJSON builds a minimal `docker inspect` array with the standard
// published ports so it passes drift detection by default.
func inspectJSON(state string, running bool, image string) string {
	return fmt.Sprintf(`[{"State":{"Status":%q,"Running":%t},"Config":{"Image":%q},"Image":"sha256:deadbeef","HostConfig":{"PortBindings":{"4001/tcp":[{"HostIp":"127.0.0.1","HostPort":"4001"}],"10000/tcp":[{"HostIp":"127.0.0.1","HostPort":"10000"}],"10199/tcp":[{"HostIp":"127.0.0.1","HostPort":"10199"}]}}}]`,
		state, running, image)
}
