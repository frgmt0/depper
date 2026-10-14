package doctor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/frgmt0/depper/internal/config"
	"github.com/frgmt0/depper/internal/engine"
	"github.com/frgmt0/depper/internal/transport"
	"github.com/frgmt0/depper/internal/transport/transporttest"
)

func baseConfig() *config.Config {
	return &config.Config{
		Service: config.Service{Name: "api", Command: "./run"},
		Deploy:  config.Deploy{Host: "server"},
	}
}

const healthyFacts = "user=app\nbase=/home/app/.local/share/depper/api\nrsync=/usr/bin/rsync\nsha256sum=/usr/bin/sha256sum\n" +
	"systemctl=/usr/bin/systemctl\nsystemd=ok\nlinger=yes\ndisk_kb=52428800\nlayout=depper\nlock=free\ndoctor=1\n"

func runWith(t *testing.T, cfg *config.Config, out string, runErr error) (*Report, *transporttest.Recorder) {
	t.Helper()
	rec := &transporttest.Recorder{Handler: func(transporttest.Call) (transport.Result, error) {
		return transport.Result{Stdout: out}, runErr
	}}
	d := &Doctor{
		Config: cfg,
		Remote: transport.NewRemote(rec, cfg.SSHTarget()),
	}
	return d.Run(context.Background()), rec
}

func byName(r *Report, name string) Check {
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	return Check{Name: name, Detail: "missing"}
}

func with(facts, key, value string) string {
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(facts), "\n") {
		if !strings.HasPrefix(line, key+"=") {
			out = append(out, line)
		}
	}
	return strings.Join(append(out, key+"="+value), "\n") + "\n"
}

func TestHealthyHostPassesInOneRoundTrip(t *testing.T) {
	report, rec := runWith(t, baseConfig(), healthyFacts, nil)
	if report.Failed() {
		t.Fatalf("unexpected failure: %+v", report.Checks)
	}
	for _, c := range report.Checks {
		if c.Status != Pass {
			t.Errorf("%s = %s (%s), want pass", c.Name, c.Status, c.Detail)
		}
	}
	if calls := rec.Calls(); len(calls) != 1 {
		t.Fatalf("%d ssh calls, want exactly 1", len(calls))
	}
	if len(rec.Scripts()) != 1 {
		t.Fatal("the diagnostic script was not sent over ssh")
	}
}

func TestConnectivityFailureIsOneFailedCheck(t *testing.T) {
	report, _ := runWith(t, baseConfig(), "", &transport.CommandError{Program: "ssh", ExitCode: 255, Stderr: "Permission denied (publickey)."})
	if len(report.Checks) != 1 || report.Checks[0].Status != Fail || report.Checks[0].Name != "ssh" {
		t.Fatalf("checks = %+v", report.Checks)
	}
	if !strings.Contains(report.Checks[0].Detail, "Permission denied") {
		t.Errorf("detail = %q", report.Checks[0].Detail)
	}
	if !report.Failed() {
		t.Error("Failed() = false")
	}
}

func TestMissingMarkerIsAConnectivityFailure(t *testing.T) {
	report, _ := runWith(t, baseConfig(), "user=app\n", nil)
	if len(report.Checks) != 1 || report.Checks[0].Status != Fail {
		t.Fatalf("checks = %+v", report.Checks)
	}
}

func TestIndividualChecks(t *testing.T) {
	tests := []struct {
		name   string
		facts  string
		check  string
		status Status
		detail string
	}{
		{"no rsync", with(healthyFacts, "rsync", ""), "rsync", Fail, "not installed"},
		{"no sha256sum", with(healthyFacts, "sha256sum", ""), "sha256sum", Fail, "not installed"},
		{"no systemctl", with(with(healthyFacts, "systemctl", ""), "systemd", "fail"), "systemd", Fail, "not installed"},
		{"user manager unreachable", with(healthyFacts, "systemd", "fail"), "systemd", Fail, "cannot reach"},
		{"linger off", with(healthyFacts, "linger", "no"), "linger", Warn, "enable-linger app"},
		{"linger unknown", with(healthyFacts, "linger", "unknown"), "linger", Warn, "could not determine"},
		{"low disk", with(healthyFacts, "disk_kb", "524288"), "disk", Warn, "512 MiB"},
		{"exactly 1 GiB", with(healthyFacts, "disk_kb", "1048576"), "disk", Pass, "1.0 GiB"},
		{"bad disk", with(healthyFacts, "disk_kb", ""), "disk", Warn, "could not determine"},
		{"legacy layout", with(healthyFacts, "layout", "legacy"), "layout", Warn, "depper migrate"},
		{"both layouts", with(healthyFacts, "layout", "both"), "layout", Warn, "both"},
		{"interrupted migration", with(healthyFacts, "layout", "migrating"), "layout", Warn, "interrupted"},
		{"no deployment", with(healthyFacts, "layout", "none"), "layout", Pass, "no deployment yet"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report, _ := runWith(t, baseConfig(), tt.facts, nil)
			c := byName(report, tt.check)
			if c.Status != tt.status || !strings.Contains(c.Detail, tt.detail) {
				t.Errorf("%s = %s %q, want %s containing %q", tt.check, c.Status, c.Detail, tt.status, tt.detail)
			}
		})
	}
}

// Lock staleness uses the deploy host's clock: its `date +%s` against the
// lock directory's mtime (refreshed by running operations), never the
// client clock or the info's created_at.
func TestLockChecks(t *testing.T) {
	held := with(healthyFacts, "lock", "held")
	info := with(held, "lock_info", `{"created_at":"2020-01-01T00:00:00Z","holder":"me@laptop","pid":42,"command":"deploy"}`)
	age := func(facts string, seconds int) string {
		return with(with(facts, "lock_now", "1800000000"), "lock_mtime", strconv.Itoa(1800000000-seconds))
	}
	tests := []struct {
		name   string
		facts  string
		status Status
		detail string
	}{
		{"fresh lock (old created_at, recently refreshed)", age(info, 60), Pass, "in progress by `depper deploy` (me@laptop, pid 42) since 2020-01-01T00:00:00Z, last refreshed 1m0s ago"},
		{"stale lock", age(info, 16*60), Warn, "depper unlock"},
		{"exactly 15 minutes is stale", age(held, 15*60), Warn, "stale"},
		{"stale without info", with(held, "lock_old", "yes"), Warn, "stale"},
		{"fresh without clock facts", with(held, "lock_old", "no"), Pass, "in progress"},
		{"clock facts win over find", with(age(held, 30), "lock_old", "yes"), Pass, "in progress"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report, _ := runWith(t, baseConfig(), tt.facts, nil)
			c := byName(report, "lock")
			if c.Status != tt.status || !strings.Contains(c.Detail, tt.detail) {
				t.Errorf("lock = %s %q, want %s containing %q", c.Status, c.Detail, tt.status, tt.detail)
			}
		})
	}
}

func TestContainerAndTunnelChecks(t *testing.T) {
	cfg := baseConfig()
	cfg.Service.Command = ""
	cfg.Container = &config.Container{Engine: "podman", Image: "nginx"}
	cfg.Tunnel = &config.Tunnel{Hostname: "app.example.com", Service: "http://localhost:8080"}

	report, _ := runWith(t, cfg, healthyFacts, nil)
	if c := byName(report, "container engine"); c.Status != Fail || !strings.Contains(c.Detail, "podman") {
		t.Errorf("container engine = %+v", c)
	}
	if c := byName(report, "cloudflared"); c.Status != Fail {
		t.Errorf("cloudflared = %+v", c)
	}
	if !report.Failed() {
		t.Error("Failed() = false")
	}

	facts := with(with(healthyFacts, "engine", "/usr/bin/podman"), "cloudflared", "/usr/local/bin/cloudflared")
	report, _ = runWith(t, cfg, facts, nil)
	if report.Failed() || byName(report, "container engine").Status != Pass || byName(report, "cloudflared").Status != Pass {
		t.Errorf("checks = %+v", report.Checks)
	}

	// Without [container] / [tunnel] the checks are absent.
	report, _ = runWith(t, baseConfig(), healthyFacts, nil)
	for _, name := range []string{"container engine", "cloudflared"} {
		if c := byName(report, name); c.Detail != "missing" {
			t.Errorf("%s unexpectedly present: %+v", name, c)
		}
	}
}

func TestRemoteTextCannotInjectControlCharacters(t *testing.T) {
	report, _ := runWith(t, baseConfig(), with(healthyFacts, "rsync", "/bin/rsync\x1b[2J"), nil)
	if c := byName(report, "rsync"); strings.ContainsRune(c.Detail, 0x1b) {
		t.Errorf("detail kept an escape character: %q", c.Detail)
	}
}

func TestScriptContent(t *testing.T) {
	cfg := baseConfig()
	cfg.Container = &config.Container{Engine: "docker", Image: "x"}
	cfg.Tunnel = &config.Tunnel{Hostname: "a.example.com", Service: "http://localhost:1"}
	script := Script(cfg)
	for _, want := range []string{
		`base="$HOME/.local/share/depper/api"`,
		`legacy="$HOME/.local/share/deployer/api"`,
		"systemctl --user show-environment",
		"loginctl show-user",
		"df -Pk",
		"command -v docker",
		"command -v cloudflared",
		"deployer-api.service",
		"printf 'doctor=1\\n'",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q", want)
		}
	}
	custom := baseConfig()
	custom.Deploy.Path = "/srv/my app"
	if !strings.Contains(Script(custom), "base='/srv/my app'") {
		t.Errorf("custom path not quoted:\n%s", Script(custom))
	}
	if strings.Contains(Script(baseConfig()), "cloudflared") || strings.Contains(Script(baseConfig()), "engine=") {
		t.Error("optional probes present without [tunnel]/[container]")
	}
}

// The script only reads state, so it is safe to run locally with a throwaway
// HOME. This catches shell syntax and parsing mistakes.
func TestScriptRunsUnderPOSIXShell(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	home := t.TempDir()
	base := filepath.Join(home, ".local/share/depper/api")
	if err := os.MkdirAll(filepath.Join(base, ".depper/lock"), 0o700); err != nil {
		t.Fatal(err)
	}
	info := `{"created_at":"2020-01-01T00:00:00Z","holder":"u@h","pid":7,"command":"deploy"}`
	if err := os.WriteFile(filepath.Join(base, ".depper/lock/info"), []byte(info+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sh, "-s")
	cmd.Env = scriptEnv(t, home)
	cmd.Stdin = strings.NewReader(Script(baseConfig()))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	facts := ParseFacts(string(out))
	if facts["doctor"] != "1" || facts["base"] != base || facts["layout"] != "depper" || facts["lock"] != "held" || facts["lock_info"] == "" {
		t.Errorf("facts = %v", facts)
	}
	if facts["lock_old"] != "no" {
		t.Errorf("lock_old = %q (directory was just created)", facts["lock_old"])
	}
	if age, stale := engine.LockAge(facts["lock_now"], facts["lock_mtime"], facts["lock_old"]); age < 0 || age > 60 || stale {
		t.Errorf("lock age = %d stale=%v from %v", age, stale, facts)
	}
	// The info says 2020, but the directory was just created: not stale.
	report := (&Doctor{Config: baseConfig()}).evaluate(facts)
	for _, c := range report {
		if c.Name == "lock" && c.Status != Pass {
			t.Errorf("lock = %+v, want an in-progress lock (host clock and directory mtime)", c)
		}
	}
}

func TestParseFacts(t *testing.T) {
	facts := ParseFacts("a=1\nnoise\nb=x=y\r\na=2\n=bad\n")
	if facts["a"] != "2" || facts["b"] != "x=y" || len(facts) != 2 {
		t.Errorf("facts = %v", facts)
	}
}

// Doctor classifies layouts exactly like the engine's probe: the
// compatibility link left by `depper migrate` is not a legacy layout, a
// moved-but-not-renamed base is legacy, and a migration marker means an
// interrupted migration.
func TestScriptLayoutDetection(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	run := func(t *testing.T, home string) map[string]string {
		t.Helper()
		cmd := exec.Command(sh, "-s")
		cmd.Env = scriptEnv(t, home)
		cmd.Stdin = strings.NewReader("echo 'motd: layout=legacy'\n" + Script(baseConfig()))
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("script failed: %v\n%s", err, out)
		}
		return ParseFacts(string(out))
	}
	mkdir := func(t *testing.T, p string) {
		t.Helper()
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	home := t.TempDir()
	base := filepath.Join(home, ".local/share/depper/api")
	mkdir(t, filepath.Join(base, ".depper"))
	mkdir(t, filepath.Join(home, ".local/share/deployer"))
	if err := os.Symlink("../depper/api", filepath.Join(home, ".local/share/deployer/api")); err != nil {
		t.Fatal(err)
	}
	if got := run(t, home)["layout"]; got != "depper" {
		t.Errorf("compatibility link: layout = %q", got)
	}
	if err := os.WriteFile(filepath.Join(base, ".depper", "migration-in-progress"), []byte("app_active=yes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := run(t, home)["layout"]; got != "migrating" {
		t.Errorf("marker: layout = %q", got)
	}
	if err := os.RemoveAll(filepath.Join(base, ".depper")); err != nil {
		t.Fatal(err)
	}
	mkdir(t, filepath.Join(base, ".deployer"))
	if got := run(t, home)["layout"]; got != "legacy" {
		t.Errorf("moved base: layout = %q", got)
	}
}

// Text a login shell prints before the facts marker is ignored.
func TestFactsAfterMarkerOnly(t *testing.T) {
	report, _ := runWith(t, baseConfig(), "doctor=1\nlayout=legacy\nrsync=\n@@doctor\n"+healthyFacts, nil)
	if c := byName(report, "layout"); c.Status != Pass {
		t.Errorf("layout = %+v", c)
	}
	if c := byName(report, "rsync"); c.Status != Pass {
		t.Errorf("rsync = %+v", c)
	}
	if !strings.Contains(Script(baseConfig()), "echo @@doctor\n") {
		t.Error("script prints no facts marker")
	}
}

func composeConfig(engineName string) *config.Config {
	cfg := baseConfig()
	cfg.Service.Command = ""
	cfg.Container = &config.Container{Engine: engineName, Compose: "compose.yaml"}
	return cfg
}

const composeHint = "(install a compose provider: podman-compose for podman, the docker compose plugin for docker; `depper host init` installs one)"

func TestComposeCheck(t *testing.T) {
	engineFacts := with(healthyFacts, "engine", "/usr/bin/podman")

	report, _ := runWith(t, composeConfig("podman"), with(with(engineFacts, "compose_rc", "0"), "compose_out", "podman-compose version 1.0.6"), nil)
	if c := byName(report, "compose"); c.Status != Pass || c.Detail != "podman-compose version 1.0.6; provider podman-compose (named by its version line), which runs podman itself (no API socket needed)" {
		t.Errorf("compose = %+v", c)
	}
	if report.Failed() {
		t.Errorf("checks = %+v", report.Checks)
	}

	report, _ = runWith(t, composeConfig("podman"), with(with(engineFacts, "compose_rc", "125"), "compose_out", "Error: looking up compose provider failed"), nil)
	want := "`podman compose version` failed: Error: looking up compose provider failed " + composeHint
	if c := byName(report, "compose"); c.Status != Fail || c.Detail != want {
		t.Errorf("compose = %+v, want detail %q", c, want)
	}
	if !report.Failed() {
		t.Error("Failed() = false with a failing compose check")
	}

	// No output: the exit status is the message.
	report, _ = runWith(t, composeConfig("docker"), with(with(with(healthyFacts, "engine", "/usr/bin/docker"), "compose_rc", "1"), "compose_out", ""), nil)
	if c := byName(report, "compose"); c.Status != Fail || c.Detail != "`docker compose version` failed: exit status 1 "+composeHint {
		t.Errorf("compose = %+v", c)
	}

	// Remote text is cleaned and bounded.
	long := "v1\x1b[2J" + strings.Repeat("x", 500)
	report, _ = runWith(t, composeConfig("docker"), with(with(with(healthyFacts, "engine", "/usr/bin/docker"), "compose_rc", "0"), "compose_out", long), nil)
	if c := byName(report, "compose"); c.Status != Pass || strings.ContainsRune(c.Detail, 0x1b) || len([]rune(c.Detail)) != maxComposeLine {
		t.Errorf("compose = %+v", c)
	}

	// Missing engine: only the container engine check fails; compose is
	// skipped rather than reported twice.
	report, _ = runWith(t, composeConfig("podman"), healthyFacts, nil)
	if c := byName(report, "container engine"); c.Status != Fail {
		t.Errorf("container engine = %+v", c)
	}
	if c := byName(report, "compose"); c.Detail != "missing" {
		t.Errorf("compose checked without an engine: %+v", c)
	}

	// Build/image modes do not use compose.
	cfg := baseConfig()
	cfg.Service.Command = ""
	cfg.Container = &config.Container{Engine: "podman", Image: "nginx"}
	report, _ = runWith(t, cfg, with(with(engineFacts, "compose_rc", "1"), "compose_out", "nope"), nil)
	if c := byName(report, "compose"); c.Detail != "missing" {
		t.Errorf("compose checked in image mode: %+v", c)
	}
	if strings.Contains(Script(cfg), "compose") {
		t.Error("image-mode script probes compose")
	}
	if script := Script(composeConfig("docker")); !strings.Contains(script, "docker compose version 2>&1 </dev/null") {
		t.Errorf("compose-mode script lacks the probe:\n%s", script)
	}
}

// The compose probe runs under a POSIX shell against fake engines: a working
// provider (after podman's provider banner), a failing one, and none.
func TestComposeProbeRunsUnderPOSIXShell(t *testing.T) {
	for _, tt := range []struct {
		name, engine, fake string
		status             Status
		detail             string
	}{
		{"working", "podman", "#!/bin/sh\ntest \"$1 $2\" = 'compose version' || exit 9\necho '>>>> Executing external compose provider \"/usr/bin/podman-compose\". Please refer to the documentation for details. <<<<' >&2\necho >&2\necho 'podman-compose version 1.0.6'\necho 'podman version 4.9.3'\n", Pass, "podman-compose version 1.0.6; provider /usr/bin/podman-compose (podman-compose), which runs podman itself (no API socket needed)"},
		{"failing", "docker", "#!/bin/sh\necho \"docker: '$1' is not a docker command.\" >&2\necho \"See 'docker --help'\" >&2\nexit 1\n", Fail, "`docker compose version` failed: docker: 'compose' is not a docker command. " + composeHint},
		{"no engine", "podman", "", "", ""},
	} {
		home := t.TempDir()
		env := scriptEnv(t, home)
		if tt.fake != "" {
			bin := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, tt.engine), []byte(tt.fake), 0o755); err != nil {
				t.Fatal(err)
			}
			env[1] = "PATH=" + bin + string(os.PathListSeparator) + strings.TrimPrefix(env[1], "PATH=")
		}
		cmd := exec.Command("/bin/sh", "-s")
		cmd.Env = env
		cmd.Stdin = strings.NewReader(Script(composeConfig(tt.engine)))
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s: script failed: %v\n%s", tt.name, err, out)
		}
		facts := ParseFacts(string(out))
		if facts["doctor"] != "1" {
			t.Fatalf("%s: script did not finish: %q", tt.name, out)
		}
		c := Check{Detail: "missing"}
		for _, check := range (&Doctor{Config: composeConfig(tt.engine)}).evaluate(facts) {
			if check.Name == "compose" {
				c = check
			}
		}
		if tt.fake == "" {
			if c.Detail != "missing" {
				t.Errorf("%s: compose = %+v", tt.name, c)
			}
			continue
		}
		if c.Status != tt.status || c.Detail != tt.detail {
			t.Errorf("%s: compose = %+v, want %s %q (facts %v)", tt.name, c, tt.status, tt.detail, facts)
		}
	}
}

// podman runs docker-compose (when docker is installed) as its compose
// provider, which talks to the user's podman API socket. `podman compose
// version` works without the socket, so the check looks at the provider
// and podman.socket instead of passing on the version alone.
func TestPodmanComposeProviderNeedsSocket(t *testing.T) {
	plugin := "/usr/lib/docker/cli-plugins/docker-compose"
	facts := func(provider, version, enabled, active string) string {
		f := with(with(healthyFacts, "engine", "/usr/bin/podman"), "compose_rc", "0")
		f = with(with(f, "compose_out", version), "compose_provider", provider)
		return with(with(f, "podman_socket_enabled", enabled), "podman_socket_active", active)
	}
	for _, tt := range []struct {
		name   string
		facts  string
		status Status
		detail []string
	}{
		{"docker-compose, socket missing", facts(plugin, "Docker Compose version v2.29.7", "", "inactive"), Fail,
			[]string{"provider " + plugin + " (docker-compose)", "podman.socket is not found for app", "systemctl --user enable --now podman.socket"}},
		{"docker-compose, socket masked", facts(plugin, "Docker Compose version v2.29.7", "masked", "inactive"), Fail,
			[]string{"podman.socket is masked for app", "unmask"}},
		{"docker-compose, socket disabled", facts(plugin, "Docker Compose version v2.29.7", "disabled", "inactive"), Warn,
			[]string{"podman.socket is disabled for app", "the next `depper deploy` enables it"}},
		{"docker-compose, socket enabled", facts(plugin, "Docker Compose version v2.29.7", "enabled", "inactive"), Pass,
			[]string{"podman.socket is enabled for app"}},
		{"docker-compose, socket active", facts(plugin, "Docker Compose version v2.29.7", "disabled", "active"), Pass,
			[]string{"podman.socket is active for app"}},
		{"docker-compose named by its version line only", facts("", "Docker Compose version v2.29.7", "", ""), Fail,
			[]string{"provider docker-compose (named by its version line)", "podman.socket is not found"}},
		{"podman-compose", facts("/usr/bin/podman-compose", "podman-compose version 1.2.0", "", ""), Pass,
			[]string{"provider /usr/bin/podman-compose (podman-compose)", "no API socket needed"}},
		{"unknown provider, no socket", facts("", "v9", "disabled", "inactive"), Warn,
			[]string{"did not name its compose provider", "if it is docker-compose, it needs podman.socket, which is disabled"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			report, _ := runWith(t, composeConfig("podman"), tt.facts, nil)
			c := byName(report, "compose")
			if c.Status != tt.status {
				t.Errorf("compose = %s %q, want %s", c.Status, c.Detail, tt.status)
			}
			for _, want := range tt.detail {
				if !strings.Contains(c.Detail, want) {
					t.Errorf("compose detail %q lacks %q", c.Detail, want)
				}
			}
		})
	}
	// docker compose never looks at podman.socket.
	if strings.Contains(Script(composeConfig("docker")), "podman.socket") {
		t.Error("docker compose script probes podman.socket")
	}
	if !strings.Contains(Script(composeConfig("podman")), "systemctl --user is-enabled podman.socket") {
		t.Error("podman compose script does not probe podman.socket")
	}
}

// The provider and socket probes run under a POSIX shell: podman's banner
// names docker-compose, and a fake systemctl reports the socket state.
func TestPodmanComposeProbeUnderPOSIXShell(t *testing.T) {
	podman := "#!/bin/sh\ntest \"$1 $2\" = 'compose version' || exit 9\n" +
		"echo '>>>> Executing external compose provider \"/usr/lib/docker/cli-plugins/docker-compose\". Please see podman-compose(1) for how to disable this message. <<<<' >&2\necho >&2\n" +
		"echo 'Docker Compose version v2.29.7'\n"
	for _, tt := range []struct {
		state  string // "" = podman.socket not found
		status Status
	}{
		{"", Fail}, {"disabled", Warn}, {"enabled", Pass},
	} {
		home := t.TempDir()
		env := scriptEnv(t, home)
		bin := t.TempDir()
		systemctl := "#!/bin/sh\ntest \"$1\" = --user || exit 1\ncase \"$2\" in\n" +
			"  is-enabled) s=" + shellQuote(tt.state) + "; if test -z \"$s\"; then echo 'Failed to get unit file state for podman.socket: No such file or directory' >&2; exit 1; fi; echo \"$s\"; test \"$s\" = enabled ;;\n" +
			"  is-active) echo inactive; exit 3 ;;\n  *) exit 1 ;;\nesac\n"
		for name, body := range map[string]string{"podman": podman, "systemctl": systemctl} {
			if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		env[1] = "PATH=" + bin + string(os.PathListSeparator) + strings.TrimPrefix(env[1], "PATH=")
		cmd := exec.Command("/bin/sh", "-s")
		cmd.Env = env
		cmd.Stdin = strings.NewReader(Script(composeConfig("podman")))
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("script failed: %v\n%s", err, out)
		}
		facts := ParseFacts(string(out))
		if facts["compose_provider"] != "/usr/lib/docker/cli-plugins/docker-compose" || facts["compose_out"] != "Docker Compose version v2.29.7" || facts["podman_socket_enabled"] != tt.state {
			t.Fatalf("state %q: facts = %v", tt.state, facts)
		}
		status, detail := composeCheck("podman", "app", facts)
		if status != tt.status {
			t.Errorf("state %q: compose = %s %q, want %s", tt.state, status, detail, tt.status)
		}
	}
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'" }

// A systemd user manager started before the deploy user joined the docker
// group keeps running without it, so its services cannot reach docker
// while SSH logins can.
func TestDockerGroupCheck(t *testing.T) {
	base := with(with(with(healthyFacts, "engine", "/usr/bin/docker"), "docker_gid", "998"), "uid", "1003")
	for _, tt := range []struct {
		name   string
		facts  string
		status Status
		detail string
	}{
		{"manager has the group", with(with(with(base, "session_groups", "1003 998"), "manager_groups", "1003 998"), "manager_groups_read", "1"), Pass, "user@1003.service has the docker group (gid 998)"},
		{"manager lacks the group", with(with(with(base, "session_groups", "1003 998"), "manager_groups", "1003"), "manager_groups_read", "1"), Fail, "service manager lacks docker group: user@1003.service"},
		{"manager unreadable", with(base, "session_groups", "1003 998"), Warn, "could not read the groups"},
		{"login lacks the group", with(base, "session_groups", "1003"), Warn, "not in the docker group (gid 998)"},
		{"no docker group", with(with(healthyFacts, "engine", "/usr/bin/docker"), "session_groups", "1003"), Warn, "no docker group"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Service.Command = ""
			cfg.Container = &config.Container{Engine: "docker", Image: "nginx"}
			report, _ := runWith(t, cfg, tt.facts, nil)
			c := byName(report, "docker group")
			if c.Status != tt.status || !strings.Contains(c.Detail, tt.detail) {
				t.Errorf("docker group = %s %q, want %s containing %q", c.Status, c.Detail, tt.status, tt.detail)
			}
			if tt.status == Fail && (!strings.Contains(c.Detail, "depper host init") || !strings.Contains(c.Detail, "systemctl restart user@1003.service")) {
				t.Errorf("no fix in %q", c.Detail)
			}
		})
	}
	podman := baseConfig()
	podman.Container = &config.Container{Engine: "podman", Image: "nginx"}
	if strings.Contains(Script(podman), "manager_groups") {
		t.Error("podman script probes the docker group")
	}
}

// DockerGroupLines run under a POSIX shell against a fake /etc/group and
// /proc and a systemctl that reports the manager's pid.
func TestDockerGroupProbeUnderPOSIXShell(t *testing.T) {
	gids := strings.Fields(runOut(t, "id", "-G"))
	if len(gids) == 0 {
		t.Skip("id -G printed nothing")
	}
	gid := gids[0]
	for _, tt := range []struct {
		managerGroups string
		status        Status
	}{
		{"0 " + gid, Pass}, {"0", Fail},
	} {
		root := t.TempDir()
		for rel, body := range map[string]string{
			"etc/group":        "root:x:0:\ndocker:x:" + gid + ":app\n",
			"proc/4242/status": "Name:\tsystemd\nGroups:\t" + tt.managerGroups + " \nVmPeak:\t1 kB\n",
		} {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, rel), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		bin := t.TempDir()
		systemctl := "#!/bin/sh\ncase \"$*\" in \"show -p MainPID --value user@\"*.service) echo 4242 ;; *) exit 1 ;; esac\n"
		if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(systemctl), 0o755); err != nil {
			t.Fatal(err)
		}
		env := scriptEnv(t, t.TempDir())
		env[1] = "PATH=" + bin + string(os.PathListSeparator) + strings.TrimPrefix(env[1], "PATH=")
		cmd := exec.Command("/bin/sh", "-s")
		cmd.Env = env
		cmd.Stdin = strings.NewReader(strings.Join(append([]string{"set -u"}, DockerGroupLines(root)...), "\n") + "\n")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("probe failed: %v\n%s", err, out)
		}
		facts := ParseFacts(string(out))
		if facts["docker_gid"] != gid || facts["manager_pid"] != "4242" || facts["manager_groups_read"] != "1" {
			t.Fatalf("facts = %v", facts)
		}
		if status, detail := DockerGroupCheck("app", facts); status != tt.status {
			t.Errorf("manager groups %q: %s %q, want %s", tt.managerGroups, status, detail, tt.status)
		}
	}
}

func runOut(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		t.Skipf("%s: %v", name, err)
	}
	return string(out)
}
