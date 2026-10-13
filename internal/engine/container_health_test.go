package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const containerToml = `[service]
name = "web"

[deploy]
host = "fakehost"
keep_releases = 2

[env]
PORT = "8080"

[container]
build = "."
dockerfile = "Dockerfile"
ports = ["127.0.0.1:8080:8080"]
volumes = ["/srv/data:/data:ro"]
args = ["--memory", "256m"]
command = "serve --label 'a b'"
`

func TestContainerBuildDeployAndImagePruning(t *testing.T) {
	h := newFakeHost(t)
	repo := writeRepo(t, containerToml, map[string]string{"Dockerfile": "FROM scratch\n", ".env": "TOKEN=" + sentinel + "\n"})
	e := h.Engine(loadConfig(t, repo))
	ctx := context.Background()
	r1 := deploy(t, e, DeployOptions{Env: EnvSource{File: filepath.Join(repo, ".env")}})
	docker := h.Root + "/remote-bin/docker"
	unitFile := h.read(".config/systemd/user/depper-web.service")
	for _, want := range []string{
		`ExecStartPre=-"` + docker + `" "rm" "-f" "depper-web"`,
		`ExecStart="` + docker + `" "run" "--rm" "--name" "depper-web" "--env-file" "` + h.path(".local/share/depper/web") + `/.depper/container.env" "-p" "127.0.0.1:8080:8080" "-v" "/srv/data:/data:ro" "--memory" "256m" "depper/web:` + r1.Release + `" "serve" "--label" "a b"`,
		`ExecStop="` + docker + `" "stop" "depper-web"`,
	} {
		if !strings.Contains(unitFile, want) {
			t.Errorf("unit missing %s\n%s", want, unitFile)
		}
	}
	if strings.Contains(unitFile, "EnvironmentFile=") {
		t.Error("build-mode unit should pass the environment with --env-file only")
	}
	cenv := h.read(".local/share/depper/web/.depper/container.env")
	if cenv != "PORT=8080\nTOKEN="+sentinel+"\n" || h.mode(".local/share/depper/web/.depper/container.env") != 0o600 {
		t.Errorf("container.env = %q", cenv)
	}
	if !strings.Contains(h.log("docker.log"), "build -t depper/web:"+r1.Release+" -f Dockerfile .") {
		t.Errorf("docker log = %s", h.log("docker.log"))
	}
	r2 := deploy(t, e, DeployOptions{Env: EnvSource{File: filepath.Join(repo, ".env")}})
	r3 := deploy(t, e, DeployOptions{Env: EnvSource{File: filepath.Join(repo, ".env")}})
	if !strings.Contains(h.log("docker.log"), "rmi depper/web:"+r1.Release) {
		t.Errorf("image of pruned release not removed: %s", h.log("docker.log"))
	}
	images := h.log("docker-images")
	if strings.Contains(images, r1.Release) || !strings.Contains(images, r2.Release) || !strings.Contains(images, r3.Release) {
		t.Errorf("images = %q", images)
	}
	// Rollback in build mode reuses the release's image tag.
	if _, err := e.Rollback(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.read(".config/systemd/user/depper-web.service"), "depper/web:"+r2.Release) {
		t.Error("rollback did not restore the release's image")
	}
	// Multi-line values are rejected before any remote command.
	h.Exec.calls, h.Exec.stdin = nil, nil
	_, err := e.Deploy(ctx, DeployOptions{Env: EnvSource{Stdin: strings.NewReader("KEY=\"a\nb\"\n")}})
	if err == nil || !strings.Contains(err.Error(), "KEY has a multi-line value") || len(h.Exec.Argv()) != 0 {
		t.Errorf("multiline err = %v, calls %v", err, h.Exec.Argv())
	}
	checkScriptSyntax(t, h.Exec.Scripts())
}

func TestComposeUnit(t *testing.T) {
	h := newFakeHost(t)
	toml := "[service]\nname = \"Shop.App\"\n[deploy]\nhost = \"fakehost\"\n[container]\nengine = \"podman\"\ncompose = \"compose.yaml\"\n"
	repo := writeRepo(t, toml, map[string]string{"compose.yaml": "services: {}\n"})
	cfg := loadConfig(t, repo)
	h.writeExec("remote-bin/podman", strings.Replace(fakeDocker, "docker.log", "podman.log", 1))
	e := h.Engine(cfg)
	deploy(t, e, DeployOptions{})
	podman := h.Root + "/remote-bin/podman"
	unitFile := h.read(".config/systemd/user/depper-Shop.App.service")
	envFile := h.path(".local/share/depper/Shop.App") + "/.depper/container.env"
	for _, want := range []string{
		"EnvironmentFile=" + h.path(".local/share/depper/Shop.App") + "/.depper/environment",
		`ExecStart="` + podman + `" "compose" "-p" "depper-shop_app" "-f" "compose.yaml" "--env-file" "` + envFile + `" "up" "--build" "--remove-orphans"`,
		`ExecStop="` + podman + `" "compose" "-p" "depper-shop_app" "-f" "compose.yaml" "down"`,
	} {
		if !strings.Contains(unitFile, want) {
			t.Errorf("unit missing %s\n%s", want, unitFile)
		}
	}
	if !strings.Contains(h.log("podman.log"), "compose -p depper-shop_app -f compose.yaml --env-file") {
		t.Errorf("podman log = %s", h.log("podman.log"))
	}
}

func TestContainerEngineMissing(t *testing.T) {
	h := newFakeHost(t)
	repo := writeRepo(t, strings.Replace(containerToml, "[container]", "[container]\nengine = \"podman\"", 1), map[string]string{"Dockerfile": "x"})
	e := h.Engine(loadConfig(t, repo))
	if _, err := e.Deploy(context.Background(), DeployOptions{}); err == nil || !strings.Contains(err.Error(), "podman is not installed") {
		t.Errorf("err = %v", err)
	}
}

const urlToml = `[service]
name = "api"
command = "x"
[deploy]
host = "fakehost"
[health]
url = "http://127.0.0.1:8080/health"
timeout = 2
`

func TestURLHealthOverSSHForwarding(t *testing.T) {
	h := newFakeHost(t)
	repo := writeRepo(t, urlToml, map[string]string{"x": ""})
	e := h.Engine(loadConfig(t, repo))
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(h.Root, "http-status"), []byte("200"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.Root, "http-body"), []byte(`{"state":"running","progress":0.4}`), 0o644); err != nil {
		t.Fatal(err)
	}
	res := deploy(t, e, DeployOptions{})
	if !res.Health.OK || res.Health.Method != MethodForward || res.Health.Status != 200 || string(res.Health.Body) != `{"state":"running","progress":0.4}` {
		t.Errorf("health = %+v body %s", res.Health, res.Health.Body)
	}
	if got := h.log("http-last-request"); got != "127.0.0.1:8080 GET /health" {
		t.Errorf("request = %q", got)
	}
	l := mustProbe(t, e)
	// Non-2xx is unhealthy; a plain-text body is kept as a string.
	_ = os.WriteFile(filepath.Join(h.Root, "http-status"), []byte("503"), 0o644)
	_ = os.WriteFile(filepath.Join(h.Root, "http-body"), []byte("warming up"), 0o644)
	r, err := e.CheckHealth(ctx, l)
	if err != nil || r.OK || r.Status != 503 || string(r.Body) != `"warming up"` || !strings.Contains(r.Error, "2xx") {
		t.Errorf("503 = %+v (%v)", r, err)
	}
	// expect_status is honoured.
	e.Config.Health.ExpectStatus = 503
	if r, _ := e.CheckHealth(ctx, l); !r.OK {
		t.Errorf("expect_status 503 = %+v", r)
	}
	e.Config.Health.ExpectStatus = 0
	// A refused connection is unhealthy, not an error.
	_ = os.Remove(filepath.Join(h.Root, "http-status"))
	r, err = e.CheckHealth(ctx, l)
	if err != nil || r.OK || !strings.Contains(r.Error, "Connection refused") {
		t.Errorf("refused = %+v (%v)", r, err)
	}
}

const fakeCurl = `#!/bin/sh
out=""; fmt=""
while test $# -gt 0; do
  case "$1" in
    -o) out=$2; shift ;;
    -w) fmt=$2; shift ;;
    --max-time) shift ;;
    -sS) ;;
    *) url=$1 ;;
  esac
  shift
done
printf '%s' "$url" > "$FAKE_ROOT/curl-url"
if test -f "$FAKE_ROOT/curl-fail"; then echo "curl: (7) Failed to connect" >&2; printf '000 0.001'; exit 7; fi
printf '{"ok":true}' > "$out"
printf '204 0.025'
`

func TestURLHealthFallsBackToRemoteCurl(t *testing.T) {
	h := newFakeHost(t)
	repo := writeRepo(t, urlToml, map[string]string{"x": ""})
	e := h.Engine(loadConfig(t, repo))
	ctx := context.Background()
	h.touch("../forward-prohibited")
	l := &Layout{Base: h.path(base), State: LayoutDepper}

	h.writeExec("remote-bin/curl", fakeCurl)
	r, err := e.CheckHealth(ctx, l)
	if err != nil || !r.OK || r.Method != MethodCurl || r.Status != 204 || r.LatencyMS != 25 || string(r.Body) != `{"ok":true}` {
		t.Fatalf("curl result = %+v (%v)", r, err)
	}
	if h.log("curl-url") != "http://127.0.0.1:8080/health" {
		t.Errorf("curl url = %q", h.log("curl-url"))
	}
	h.touch("../curl-fail")
	r, err = e.CheckHealth(ctx, l)
	if err != nil || r.OK || !strings.Contains(r.Error, "Failed to connect") {
		t.Errorf("curl failure = %+v (%v)", r, err)
	}
	checkScriptSyntax(t, h.Exec.Scripts())
}

func TestCurlMissingErrorNamesBothFixes(t *testing.T) {
	// Parse-level check of the "missing" marker (portable: the real host
	// may have curl installed).
	h := newFakeHost(t)
	repo := writeRepo(t, urlToml, map[string]string{"x": ""})
	e := h.Engine(loadConfig(t, repo))
	e.forwardingProhibited = true
	e.Remote.Exec = scriptedExec(func(script string) (string, error) { return "@@missing\n", nil })
	_, err := e.CheckHealth(context.Background(), &Layout{Base: "/srv/api", State: LayoutDepper})
	if err == nil || !strings.Contains(err.Error(), "AllowTcpForwarding") || !strings.Contains(err.Error(), "install curl") {
		t.Errorf("err = %v", err)
	}
}

type fakeTunnel struct {
	ensureErr error
	asserted  int
	ensured   int
}

func (f *fakeTunnel) AssertNotOrphaned(ctx context.Context, e *Engine, l *Layout) error {
	f.asserted++
	return nil
}

func (f *fakeTunnel) Ensure(ctx context.Context, e *Engine, l *Layout) error {
	f.ensured++
	return f.ensureErr
}

func TestTunnelHooks(t *testing.T) {
	t.Cleanup(func() { RegisterTunnel(nil) })
	h := newFakeHost(t)
	tunnelToml := strings.Replace(processToml, "[health]", "[tunnel]\nhostname = \"api.demo.example.com\"\nservice = \"http://127.0.0.1:8080\"\n\n[health]", 1)
	repo := writeRepo(t, tunnelToml, map[string]string{"app.py": "", "healthy": "", ".env.prod": "A=1\n"})
	e := h.Engine(loadConfig(t, repo))
	ctx := context.Background()

	// [tunnel] without a registered implementation fails before any remote command.
	RegisterTunnel(nil)
	if _, err := e.Deploy(ctx, DeployOptions{}); !errors.Is(err, errNoTunnelSupport) || len(h.Exec.Argv()) != 0 {
		t.Fatalf("err = %v, calls %v", err, h.Exec.Argv())
	}

	hooks := &fakeTunnel{ensureErr: errors.New("DNS route conflict")}
	RegisterTunnel(hooks)
	// A dry run never calls the hooks or the host.
	if _, err := e.Deploy(ctx, DeployOptions{DryRun: true}); err != nil || hooks.asserted+hooks.ensured != 0 || strings.Contains(strings.Join(h.Exec.Argv(), "\n"), "ssh") {
		t.Fatalf("dry run: %v, hooks %+v, calls %v", err, hooks, h.Exec.Argv())
	}
	_, err := e.Deploy(ctx, DeployOptions{})
	if err == nil || err.Error() != "Application deployed successfully, but tunnel setup failed: DNS route conflict" {
		t.Fatalf("err = %v", err)
	}
	if hooks.ensured != 1 || hooks.asserted != 0 || h.exists(base+"/.depper/lock") || !h.exists(base+"/current") {
		t.Errorf("ensured %d asserted %d lock %v", hooks.ensured, hooks.asserted, h.exists(base+"/.depper/lock"))
	}

	// Without [tunnel], an owned tunnel.json refuses the deploy (built-in
	// check when no implementation is registered).
	RegisterTunnel(nil)
	writeFile(t, h.path(base+"/.depper/tunnel.json"), `{"name":"depper-api"}`)
	plain := h.Engine(loadConfig(t, processRepo(t)))
	if _, err := plain.Deploy(ctx, DeployOptions{}); !errors.Is(err, errOrphanedTunnel) {
		t.Errorf("orphan err = %v", err)
	}
	RegisterTunnel(hooks)
	if _, err := plain.Deploy(ctx, DeployOptions{}); err != nil || hooks.asserted != 1 {
		t.Errorf("registered AssertNotOrphaned not used: %v (%d)", err, hooks.asserted)
	}
}

func TestCommandHealthTimeoutIsEnforcedOnTheHost(t *testing.T) {
	h := newFakeHost(t)
	repo := processRepo(t)
	e := h.Engine(loadConfig(t, repo))
	deploy(t, e, DeployOptions{})
	h.writeExec("remote-bin/timeout", "#!/bin/sh\nprintf '%s' \"$1\" > \"$FAKE_ROOT/timeout-arg\"\nif test -f \"$FAKE_ROOT/timeout-expire\"; then exit 124; fi\nshift\nexec \"$@\"\n")
	l := mustProbe(t, e)
	r, err := e.CheckHealth(context.Background(), l)
	if err != nil || !r.OK || h.log("timeout-arg") != "5" {
		t.Fatalf("health = %+v (%v), timeout arg %q", r, err, h.log("timeout-arg"))
	}
	h.touch("../timeout-expire")
	r, err = e.CheckHealth(context.Background(), l)
	if err != nil || r.OK || r.Error != "health command timed out after 5s" {
		t.Errorf("expired = %+v (%v)", r, err)
	}
}
