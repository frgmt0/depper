package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
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
	repo := writeRepo(t, containerToml, map[string]string{"Dockerfile": "FROM scratch\n", ".env": "TOKEN=" + sentinel + "\nMULTI=\"line one\nline two\"\nQUOTED='\"starts with a quote'\n"})
	e := h.Engine(loadConfig(t, repo))
	ctx := context.Background()
	// Images of another service whose name lowercases to the same
	// repository ("Web"), and an unlabelled one, must survive pruning.
	foreign := "depper/web:release-20200101T000000Z-0000beef depper.service=Web\ndepper/web:release-20200101T000000Z-0000cafe -\n"
	writeFile(t, filepath.Join(h.Root, "docker-images"), foreign)
	r1 := deploy(t, e, DeployOptions{Env: EnvSource{File: filepath.Join(repo, ".env")}})
	docker := h.Root + "/remote-bin/docker"
	unitFile := h.read(".config/systemd/user/depper-web.service")
	for _, want := range []string{
		"EnvironmentFile=" + h.path(".local/share/depper/web") + "/.depper/environment",
		`ExecStartPre=-` + engineWrap + `"` + docker + `" "rm" "-f" "depper-web"`,
		`ExecStart=` + engineWrap + `"` + docker + `" "run" "--rm" "--name" "depper-web" $DEPPER_CONTAINER_ENV_ARGS "-p" "127.0.0.1:8080:8080" "-v" "/srv/data:/data:ro" "--memory" "256m" "depper/web:` + r1.Release + `" "serve" "--label" "a b"`,
		`ExecStop=` + engineWrap + `"` + docker + `" "stop" "depper-web"`,
	} {
		if !strings.Contains(unitFile, want) {
			t.Errorf("unit missing %s\n%s", want, unitFile)
		}
	}
	if strings.Contains(unitFile, "--env-file") || strings.Contains(unitFile, sentinel) {
		t.Error("unit uses an --env-file or contains a value")
	}
	// Values (multi-line, leading quotes) live only in the EnvironmentFile;
	// the container gets them by name.
	envFile := h.read(".local/share/depper/web/.depper/environment")
	for _, want := range []string{`DEPPER_CONTAINER_ENV_ARGS="--env MULTI --env PORT --env QUOTED --env TOKEN"`, "MULTI=\"line one\nline two\"", `QUOTED="\"starts with a quote"`, `TOKEN="` + sentinel + `"`} {
		if !strings.Contains(envFile, want) {
			t.Errorf("environment file lacks %q:\n%s", want, envFile)
		}
	}
	if h.exists(".local/share/depper/web/.depper/container.env") {
		t.Error("container.env is still written")
	}
	if !strings.Contains(h.log("docker.log"), "build -t depper/web:"+r1.Release+" --label depper.service=web -f Dockerfile .") {
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
	if !strings.Contains(images, foreign) {
		t.Errorf("another service's or unlabelled images were pruned: %q", images)
	}
	// Rollback in build mode reuses the release's image tag.
	if _, err := e.Rollback(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.read(".config/systemd/user/depper-web.service"), "depper/web:"+r2.Release) {
		t.Error("rollback did not restore the release's image")
	}
	// Oversized values are rejected (by name) before any remote command.
	h.Exec.calls, h.Exec.stdin = nil, nil
	_, err := e.Deploy(ctx, DeployOptions{Env: EnvSource{Stdin: strings.NewReader("BIG=" + strings.Repeat("v", 61<<10) + "\n")}})
	if err == nil || !strings.Contains(err.Error(), "BIG") || !strings.Contains(err.Error(), "60 KiB") || len(h.Exec.Argv()) != 0 {
		t.Errorf("oversized err = %v, calls %v", err, h.Exec.Argv())
	}
	checkScriptSyntax(t, h.Exec.Scripts())
}

// Image mode tags the pulled image per release, records its digest, runs
// the tag, and rolls back to the exact image of the earlier release.
func TestImageModeTagsPerRelease(t *testing.T) {
	h := newFakeHost(t)
	toml := "[service]\nname = \"web\"\n[deploy]\nhost = \"fakehost\"\nkeep_releases = 2\n[container]\nimage = \"nginx:1.25\"\n"
	repo := writeRepo(t, toml, map[string]string{"index.html": "hi\n"})
	e := h.Engine(loadConfig(t, repo))
	ctx := context.Background()
	digest := "nginx@sha256:" + strings.Repeat("ab", 32)
	writeFile(t, filepath.Join(h.Root, "image-digests"), "registry.example/other@sha256:"+strings.Repeat("cd", 32)+"\n"+digest+"\n")
	r1 := deploy(t, e, DeployOptions{})
	log := h.log("docker.log")
	if !strings.Contains(log, "pull nginx:1.25\ntag nginx:1.25 depper/web:"+r1.Release) {
		t.Errorf("docker log = %s", log)
	}
	if unitFile := h.read(".config/systemd/user/depper-web.service"); !strings.Contains(unitFile, `"depper/web:`+r1.Release+`"`) || strings.Contains(unitFile, `"nginx:1.25"`) {
		t.Errorf("unit does not run the per-release tag:\n%s", unitFile)
	}
	var meta ReleaseMetadata
	if err := json.Unmarshal([]byte(h.read(".local/share/depper/web/releases/"+r1.Release+"/"+ReleaseMetadataName)), &meta); err != nil || meta.Image != "nginx:1.25" || meta.ImageDigest != digest {
		t.Errorf("metadata = %+v (%v)", meta, err)
	}
	if r1.Metadata == nil || r1.Metadata.ImageDigest != digest {
		t.Errorf("result metadata = %+v", r1.Metadata)
	}
	r2 := deploy(t, e, DeployOptions{})
	if _, err := e.Rollback(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if unitFile := h.read(".config/systemd/user/depper-web.service"); !strings.Contains(unitFile, "depper/web:"+r1.Release) {
		t.Errorf("rollback does not run the earlier release's image:\n%s", unitFile)
	}
	deploy(t, e, DeployOptions{})
	deploy(t, e, DeployOptions{})
	if images := h.log("docker-images"); strings.Contains(images, r1.Release) || strings.Contains(images, r2.Release) {
		t.Errorf("per-release tags of pruned releases kept: %q", images)
	}
	checkScriptSyntax(t, h.Exec.Scripts())
}

// The GitHub runner image puts its own user's XDG_RUNTIME_DIR and
// XDG_CONFIG_HOME into /etc/environment, so pam_env hands /run/user/1001 and
// /home/runner/.config to every SSH login, and rootless podman failed ("XDG_RUNTIME_DIR
// directory ... is not owned by the current user", then "stat
// /home/runner/.config/containers/storage.conf: permission denied"). The
// engine must run with the deploy user's own directories.
func TestContainerEngineGetsOwnXDGDirs(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root owns every directory used here")
	}
	h := newFakeHost(t)
	writeFile(t, filepath.Join(h.Root, "login-env"), "XDG_RUNTIME_DIR=/\nXDG_CONFIG_HOME=/\n")
	toml := "[service]\nname = \"web\"\n[deploy]\nhost = \"fakehost\"\n[container]\nengine = \"podman\"\nbuild = \".\"\n"
	repo := writeRepo(t, toml, map[string]string{"Dockerfile": "FROM scratch\n"})
	h.writeExec("remote-bin/podman", fakeDocker)
	deploy(t, h.Engine(loadConfig(t, repo)), DeployOptions{})
	want := "/run/user/" + strconv.Itoa(os.Geteuid()) + " unset"
	lines := strings.Split(strings.TrimSpace(h.log("docker-xdg.log")), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatal("the container engine never ran")
	}
	for _, got := range lines {
		if got != want {
			t.Errorf("container engine ran with XDG_RUNTIME_DIR, XDG_CONFIG_HOME = %q, want %q", got, want)
		}
	}
}

func TestChooseDigest(t *testing.T) {
	a := "docker.io/library/nginx@sha256:" + strings.Repeat("a", 64)
	b := "ghcr.io/acme/api@sha256:" + strings.Repeat("b", 64)
	for _, tc := range []struct {
		image string
		lines []string
		want  string
	}{
		{"nginx:1.25", []string{b, a}, a},
		{"ghcr.io/acme/api:v1", []string{a, b}, b},
		{"localhost:5000/x", []string{"junk", "x@sha256:short"}, ""},
		{"other", []string{b}, b},
	} {
		if got := chooseDigest(tc.image, tc.lines); got != tc.want {
			t.Errorf("chooseDigest(%q) = %q, want %q", tc.image, got, tc.want)
		}
	}
}

func TestComposeUnit(t *testing.T) {
	h := newFakeHost(t)
	toml := "[service]\nname = \"Shop.App\"\n[deploy]\nhost = \"fakehost\"\n[container]\nengine = \"podman\"\ncompose = \"compose.yaml\"\n"
	repo := writeRepo(t, toml, map[string]string{"compose.yaml": "services: {}\n", ".env": "STRAY=from-dotenv-file\n"})
	cfg := loadConfig(t, repo)
	// Like podman-compose 1.0.6 (Ubuntu 24.04), reject `config --quiet`.
	noQuiet := "#!/bin/sh\ncase \" $* \" in *\" config --quiet \"*) echo \"podman-compose: error: unrecognized arguments: --quiet\" >&2; exit 2 ;; esac\n"
	h.writeExec("remote-bin/podman", noQuiet+strings.TrimPrefix(strings.Replace(fakeDocker, "docker.log", "podman.log", 1), "#!/bin/sh\n"))
	e := h.Engine(cfg)
	deploy(t, e, DeployOptions{Env: EnvSource{Stdin: strings.NewReader("TOKEN=" + sentinel + "\n")}})
	podman := h.Root + "/remote-bin/podman"
	unitFile := h.read(".config/systemd/user/depper-Shop.App.service")
	for _, want := range []string{
		"EnvironmentFile=" + h.path(".local/share/depper/Shop.App") + "/.depper/environment",
		`ExecStart=` + engineWrap + `"` + podman + `" "compose" "-p" "depper-shop_app" "-f" "compose.yaml" "--env-file" "/dev/null" "up" "--build" "--remove-orphans"`,
		`ExecStop=` + engineWrap + `"` + podman + `" "compose" "-p" "depper-shop_app" "-f" "compose.yaml" "--env-file" "/dev/null" "down"`,
	} {
		if !strings.Contains(unitFile, want) {
			t.Errorf("unit missing %s\n%s", want, unitFile)
		}
	}
	if !strings.Contains(h.log("podman.log"), "compose -p depper-shop_app -f compose.yaml --env-file /dev/null config\n") {
		t.Errorf("podman log = %s", h.log("podman.log"))
	}
	if h.exists(".local/share/depper/Shop.App/.depper/container.env") || strings.Contains(h.read(".local/share/depper/Shop.App/.depper/environment"), ContainerEnvArgsVar) {
		t.Error("compose mode wrote container.env or the --env argument list")
	}
	// The compose setup gets the values as process environment (on stdin).
	found := false
	for _, s := range h.Exec.Scripts() {
		if strings.Contains(s, " config >/dev/null") {
			found = strings.Contains(s, "export TOKEN="+sentinel)
		}
	}
	if !found {
		t.Error("compose setup does not export the environment")
	}
	for _, argv := range h.Exec.Argv() {
		if strings.Contains(argv, sentinel) {
			t.Error("a secret value reached argv")
		}
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
