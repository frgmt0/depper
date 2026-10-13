package doctor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frgmt0/depper/internal/config"
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
		Now:    func() time.Time { return time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC) },
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

func TestLockChecks(t *testing.T) {
	held := with(healthyFacts, "lock", "held")
	info := func(created string) string {
		return with(held, "lock_info", `{"created_at":"`+created+`","holder":"me@laptop","pid":42,"command":"deploy"}`)
	}
	tests := []struct {
		name   string
		facts  string
		status Status
		detail string
	}{
		{"fresh lock from info", info("2026-01-01T11:55:00Z"), Pass, "in progress by `depper deploy` (me@laptop, pid 42)"},
		{"stale lock from info", info("2026-01-01T11:30:00Z"), Warn, "depper unlock"},
		{"stale lock without info", with(held, "lock_old", "yes"), Warn, "stale"},
		{"fresh lock without info", with(held, "lock_old", "no"), Pass, "in progress"},
		{"stale dir but fresh info wins", with(with(info("2026-01-01T11:59:00Z"), "lock_old", "yes"), "lock", "held"), Pass, "in progress"},
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
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
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
	report := (&Doctor{Config: baseConfig(), Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }}).evaluate(facts)
	for _, c := range report {
		if c.Name == "lock" && c.Status != Warn {
			t.Errorf("lock = %+v, want stale warning from info created_at", c)
		}
	}
}

func TestParseFacts(t *testing.T) {
	facts := ParseFacts("a=1\nnoise\nb=x=y\r\na=2\n=bad\n")
	if facts["a"] != "2" || facts["b"] != "x=y" || len(facts) != 2 {
		t.Errorf("facts = %v", facts)
	}
}
