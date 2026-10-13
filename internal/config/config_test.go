package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

const minimal = `
[service]
name = "api"
command = "python app.py"
[deploy]
host = "server"
`

func repoWith(t *testing.T, name, contents string) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if name != "" {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func noEnv(string) (string, bool) { return "", false }

func load(t *testing.T, contents string) (*Config, error) {
	t.Helper()
	return LoadTarget(repoWith(t, FileName, contents), "", noEnv)
}

func mustLoad(t *testing.T, contents string) *Config {
	t.Helper()
	cfg, err := load(t, contents)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return cfg
}

func expectError(t *testing.T, name, contents, pattern string) {
	t.Helper()
	_, err := load(t, contents)
	if err == nil {
		t.Errorf("%s: expected an error", name)
		return
	}
	if pattern != "" && !regexp.MustCompile(pattern).MatchString(err.Error()) {
		t.Errorf("%s: error %q does not match %q", name, err, pattern)
	}
}

// Port of test_loads_valid_config_and_adds_safe_default_excludes.
func TestLoadsValidConfigAndAddsSafeDefaultExcludes(t *testing.T) {
	repo := repoWith(t, LegacyFileName, `
[service]
name = "api-worker.v2"
command = "python3 service.py --workers 2"

[deploy]
host = "deploy@host.example"
path = "/srv/apps/api"
setup = ["python3 -m venv .venv"]
exclude = ["build/", ".git/"]
healthcheck = "curl --fail http://127.0.0.1:8080/health"

[env]
PORT = "8080"
FEATURE_FLAG = "yes"
`)
	cfg, err := LoadTarget(repo, "", noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Repo != repo || cfg.File != filepath.Join(repo, LegacyFileName) {
		t.Errorf("repo/file = %q %q", cfg.Repo, cfg.File)
	}
	if cfg.UnitName() != "depper-api-worker.v2.service" {
		t.Errorf("UnitName = %q", cfg.UnitName())
	}
	if cfg.Service.Restart != "on-failure" || cfg.Deploy.Path != "/srv/apps/api" {
		t.Errorf("service/deploy = %+v %+v", cfg.Service, cfg.Deploy)
	}
	if !reflect.DeepEqual(cfg.Deploy.Setup, []string{"python3 -m venv .venv"}) {
		t.Errorf("setup = %q", cfg.Deploy.Setup)
	}
	if !reflect.DeepEqual(map[string]string(cfg.Env), map[string]string{"PORT": "8080", "FEATURE_FLAG": "yes"}) {
		t.Error("env mismatch")
	}
	count := 0
	for _, item := range cfg.Deploy.Exclude {
		if item == ".git/" {
			count++
		}
	}
	for _, want := range []string{".env", "build/", FileName, LegacyFileName, ".depper/"} {
		if !contains(cfg.Deploy.Exclude, want) {
			t.Errorf("exclude lacks %q", want)
		}
	}
	if count != 1 {
		t.Errorf(".git/ appears %d times", count)
	}
	if cfg.Health == nil || cfg.Health.Command != "curl --fail http://127.0.0.1:8080/health" || cfg.Health.URL != "" {
		t.Errorf("healthcheck alias = %+v", cfg.Health)
	}
	if cfg.Health.Timeout != 5*time.Second || cfg.Health.StartPeriod != 30*time.Second || cfg.Health.Interval != 2*time.Second || cfg.Health.ExpectStatus != 0 {
		t.Errorf("health defaults = %+v", cfg.Health)
	}
	if cfg.Deploy.KeepReleases != 5 || cfg.Deploy.Source != SourceWorktree || cfg.Deploy.Port != 0 {
		t.Errorf("deploy defaults = %+v", cfg.Deploy)
	}
	if cfg.Mode() != ModeProcess || cfg.Target != "" {
		t.Errorf("mode/target = %q %q", cfg.Mode(), cfg.Target)
	}
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

// Port of test_rejects_missing_or_unknown_configuration.
func TestRejectsMissingOrUnknownConfiguration(t *testing.T) {
	_, err := Load(repoWith(t, "", ""))
	if err == nil || !strings.Contains(err.Error(), "no depper.toml") {
		t.Errorf("missing file error = %v", err)
	}
	expectError(t, "unknown service key", `
[service]
name = "api"
command = "python api.py"
unexpected = "setting"
[deploy]
host = "server"
`, `unknown \[service\] setting "unexpected"`)
	expectError(t, "unknown section", minimal+"[extra]\nx = 1\n", `may contain only`)
	expectError(t, "invalid toml", "[service\nname=", `invalid depper.toml`)
}

func TestFileSelection(t *testing.T) {
	repo := repoWith(t, FileName, minimal)
	if err := os.WriteFile(filepath.Join(repo, LegacyFileName), []byte(minimal), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(repo); err == nil || !strings.Contains(err.Error(), "both depper.toml and deploy.toml") {
		t.Errorf("both files error = %v", err)
	}
	if err := os.Remove(filepath.Join(repo, LegacyFileName)); err != nil {
		t.Fatal(err)
	}
	p, err := Load(repo)
	if err != nil || p.File != filepath.Join(repo, FileName) {
		t.Errorf("preferred file: %v %v", p, err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	p, err = Load(link)
	if err != nil || p.Repo != repo {
		t.Errorf("symlinked repo resolves to %v (%v)", p, err)
	}
}

// Port of test_rejects_unsafe_or_invalid_values, plus the new [deploy] keys.
func TestRejectsUnsafeOrInvalidValues(t *testing.T) {
	svc := "[service]\nname = \"api\"\ncommand = \"python app.py\"\n"
	cases := map[string]string{
		"unsafe service name":           "[service]\nname = \"api; rm -rf /\"\ncommand = \"x\"\n[deploy]\nhost = \"server\"\n",
		"long service name":             "[service]\nname = \"" + strings.Repeat("a", 64) + "\"\ncommand = \"x\"\n[deploy]\nhost = \"server\"\n",
		"invalid restart":               svc + "restart = \"unless-stopped\"\n[deploy]\nhost = \"server\"\n",
		"multiline command":             "[service]\nname = \"api\"\ncommand = \"a\\nb\"\n[deploy]\nhost = \"server\"\n",
		"relative remote path":          svc + "[deploy]\nhost = \"server\"\npath = \"apps/api\"\n",
		"carriage return remote path":   svc + "[deploy]\nhost = \"server\"\npath = \"/srv/api\\rExecStart=/bin/false\"\n",
		"systemd-ambiguous remote path": svc + "[deploy]\nhost = \"server\"\npath = \"/srv/path with % sign\"\n",
		"absolute exclude":              svc + "[deploy]\nhost = \"server\"\nexclude = [\"/private-key\"]\n",
		"parent exclude":                svc + "[deploy]\nhost = \"server\"\nexclude = [\"../private-key\"]\n",
		"ssh option host":               svc + "[deploy]\nhost = \"-oProxyCommand=evil\"\n",
		"whitespace in host":            svc + "[deploy]\nhost = \"server -oProxyCommand=evil\"\n",
		"rsync separator in host":       svc + "[deploy]\nhost = \"server:2222\"\n",
		"slash in host":                 svc + "[deploy]\nhost = \"server/path\"\n",
		"option user in host":           svc + "[deploy]\nhost = \"-v@server\"\n",
		"bad ipv6":                      svc + "[deploy]\nhost = \"2001:db8::zz\"\n",
		"bracketed hostname":            svc + "[deploy]\nhost = \"[server]\"\n",
		"multiline setup":               svc + "[deploy]\nhost = \"server\"\nsetup = [\"echo one\\ntouch /tmp/surprise\"]\n",
		"nul setup":                     svc + "[deploy]\nhost = \"server\"\nsetup = [\"echo one\\u0000\"]\n",
		"carriage return healthcheck":   svc + "[deploy]\nhost = \"server\"\nhealthcheck = \"true\\rfalse\"\n",
		"carriage return environment":   svc + "[deploy]\nhost = \"server\"\n[env]\nTOKEN = \"value\\rmore\"\n",
		"unsafe environment name":       svc + "[deploy]\nhost = \"server\"\n[env]\n\"BAD-NAME\" = \"value\"\n",
		"non-string environment value":  svc + "[deploy]\nhost = \"server\"\n[env]\nPORT = 8080\n",
		"user with user@host":           svc + "[deploy]\nhost = \"deploy@server\"\nuser = \"other\"\n",
		"bad user":                      svc + "[deploy]\nhost = \"server\"\nuser = \"-oProxy\"\n",
		"port zero":                     svc + "[deploy]\nhost = \"server\"\nport = 0\n",
		"port too large":                svc + "[deploy]\nhost = \"server\"\nport = 65536\n",
		"port string":                   svc + "[deploy]\nhost = \"server\"\nport = \"22\"\n",
		"keep_releases one":             svc + "[deploy]\nhost = \"server\"\nkeep_releases = 1\n",
		"keep_releases float":           svc + "[deploy]\nhost = \"server\"\nkeep_releases = 3.5\n",
		"unknown source":                svc + "[deploy]\nhost = \"server\"\nsource = \"tarball\"\n",
		"control in identity_file":      svc + "[deploy]\nhost = \"server\"\nidentity_file = \"/k\\u0007\"\n",
		"empty known_hosts":             svc + "[deploy]\nhost = \"server\"\nknown_hosts = \" \"\n",
		"unknown deploy key":            svc + "[deploy]\nhost = \"server\"\nhosts = \"x\"\n",
		"missing host":                  svc + "[deploy]\npath = \"/srv\"\n",
		"missing command":               "[service]\nname = \"api\"\n[deploy]\nhost = \"server\"\n",
		"missing name":                  "[service]\ncommand = \"x\"\n[deploy]\nhost = \"server\"\n",
		"missing deploy":                svc,
	}
	for name, contents := range cases {
		expectError(t, name, contents, "")
	}
}

// Port of test_rejects_non_table_sections_and_non_string_lists.
func TestRejectsNonTableSectionsAndNonStringLists(t *testing.T) {
	cases := []string{
		"service = 'nope'\n[deploy]\nhost = 'server'\n",
		"[service]\nname = 'api'\ncommand = 'python app.py'\n[deploy]\nhost = 'server'\nsetup = [123]\n",
		"[service]\nname = 'api'\ncommand = 'python app.py'\n[deploy]\nhost = 'server'\nexclude = 'build/'\n",
		minimal + "env = 'x'\n",
		"targets = 'x'\n" + minimal,
	}
	for i, contents := range cases {
		expectError(t, fmt.Sprintf("case %d", i), contents, "")
	}
}

func TestAcceptsHostSpellings(t *testing.T) {
	cases := map[string]string{
		"desktop":                 "desktop",
		"host.example.com":        "host.example.com",
		"deploy@host.example":     "deploy@host.example",
		"203.0.113.10":            "203.0.113.10",
		"2001:db8::10":            "2001:db8::10",
		"[2001:db8::10]":          "2001:db8::10",
		"deploy@[2001:db8::10]":   "deploy@2001:db8::10",
		"fe80::1%eth0":            "fe80::1%eth0",
		"_svc.user@host_name.lan": "_svc.user@host_name.lan",
	}
	for in, want := range cases {
		cfg, err := load(t, "[service]\nname = \"api\"\ncommand = \"x\"\n[deploy]\nhost = \""+in+"\"\n")
		if err != nil {
			t.Errorf("host %q: %v", in, err)
			continue
		}
		if cfg.Deploy.Host != want {
			t.Errorf("host %q normalized to %q, want %q", in, cfg.Deploy.Host, want)
		}
	}
}

func TestDeployKeysAndSSHTarget(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home")
	}
	repo := repoWith(t, FileName, `
[service]
name = "api"
command = "x"
[deploy]
host = "203.0.113.10"
user = "deploy"
port = 2222
identity_file = "~/.ssh/id_ed25519"
known_hosts = "ops/known_hosts"
keep_releases = 3
source = "git"
`)
	cfg, err := LoadTarget(repo, "", noEnv)
	if err != nil {
		t.Fatal(err)
	}
	target := cfg.SSHTarget()
	if target.Host != "203.0.113.10" || target.User != "deploy" || target.Port != 2222 ||
		target.IdentityFile != filepath.Join(home, ".ssh/id_ed25519") || target.KnownHosts != filepath.Join(repo, "ops/known_hosts") {
		t.Errorf("target = %+v", target)
	}
	if cfg.Deploy.KeepReleases != 3 || cfg.Deploy.Source != SourceGit {
		t.Errorf("deploy = %+v", cfg.Deploy)
	}
}

func TestEnvOverridesForSSHCredentials(t *testing.T) {
	repo := repoWith(t, FileName, minimal+"identity_file = \"/config/key\"\n")
	lookup := func(name string) (string, bool) {
		switch name {
		case EnvSSHKeyFile:
			return "/ci/key", true
		case EnvKnownHostsKey:
			return "/ci/known_hosts", true
		}
		return "", false
	}
	cfg, err := LoadTarget(repo, "", lookup)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Deploy.IdentityFile != "/ci/key" || cfg.Deploy.KnownHosts != "/ci/known_hosts" {
		t.Errorf("overrides not applied: %+v", cfg.Deploy)
	}
	cfg, err = LoadTarget(repo, "", func(string) (string, bool) { return "", true })
	if err != nil || cfg.Deploy.IdentityFile != "/config/key" {
		t.Errorf("empty override must be ignored: %+v %v", cfg, err)
	}
	_, err = LoadTarget(repo, "", func(name string) (string, bool) { return "/k\n", name == EnvSSHKeyFile })
	if err == nil {
		t.Error("control characters in override accepted")
	}
}

// Port of test_loads_and_normalizes_a_valid_tunnel.
func TestLoadsAndNormalizesAValidTunnel(t *testing.T) {
	cfg := mustLoad(t, minimal+"[tunnel]\nhostname = \"Api.Demo.frgmt.xyz.\"\nservice = \"http://127.0.0.1:8080/\"\n")
	if cfg.Tunnel == nil || cfg.Tunnel.Hostname != "api.demo.frgmt.xyz" || cfg.Tunnel.Service != "http://127.0.0.1:8080" {
		t.Errorf("tunnel = %+v", cfg.Tunnel)
	}
	if cfg.Tunnel.URL() != "https://api.demo.frgmt.xyz" || cfg.TunnelUnitName() != "depper-tunnel@api.service" {
		t.Errorf("tunnel url/unit = %q %q", cfg.Tunnel.URL(), cfg.TunnelUnitName())
	}
}

// Port of test_rejects_invalid_tunnel_hostnames_and_origins.
func TestRejectsInvalidTunnelHostnamesAndOrigins(t *testing.T) {
	cases := []struct{ name, hostname, origin string }{
		{"wildcard hostname", "*.frgmt.xyz", "http://127.0.0.1:8080"},
		{"not enough hostname labels", "frgmt.xyz", "http://127.0.0.1:8080"},
		{"hostname with underscore", "api_bad.frgmt.xyz", "http://127.0.0.1:8080"},
		{"missing origin port", "api.demo.frgmt.xyz", "http://127.0.0.1"},
		{"credentials in origin", "api.demo.frgmt.xyz", "https://user:pass@127.0.0.1:443"},
		{"origin path", "api.demo.frgmt.xyz", "http://127.0.0.1:8080/internal"},
		{"origin query", "api.demo.frgmt.xyz", "http://127.0.0.1:8080/?debug=1"},
		{"origin tab control character", "api.demo.frgmt.xyz", `http://127.0.0.1:\t8080`},
		{"invalid origin scheme", "api.demo.frgmt.xyz", "tcp://127.0.0.1:8080"},
		{"invalid zero origin port", "api.demo.frgmt.xyz", "http://127.0.0.1:0"},
		{"origin fragment", "api.demo.frgmt.xyz", "http://127.0.0.1:8080#x"},
	}
	for _, tc := range cases {
		expectError(t, tc.name, minimal+"[tunnel]\nhostname = \""+tc.hostname+"\"\nservice = \""+tc.origin+"\"\n", "")
	}
	expectError(t, "unknown tunnel key", minimal+"[tunnel]\nhostname = \"a.b.c\"\nservice = \"http://x:1\"\nport = 1\n", `unknown \[tunnel\] setting`)
	expectError(t, "missing tunnel service", minimal+"[tunnel]\nhostname = \"a.b.c\"\n", `service`)
}

// Port of test_secrets_file_is_repository_relative_and_env_values_are_hidden_from_repr.
func TestSecretsFileIsRepositoryRelativeAndEnvValuesAreHiddenFromFmt(t *testing.T) {
	repo := repoWith(t, FileName, minimal+"[env]\nTOKEN = \"synthetic-secret-value\"\n[secrets]\nfile = \"ops/credentials.prod\"\n")
	cfg, err := LoadTarget(repo, "", noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Secrets == nil || cfg.Secrets.File != filepath.Join(repo, "ops", "credentials.prod") {
		t.Errorf("secrets = %+v", cfg.Secrets)
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		if out := fmt.Sprintf(verb, *cfg); strings.Contains(out, "synthetic-secret-value") {
			t.Errorf("%s leaks env values: %s", verb, out)
		}
		if out := fmt.Sprintf(verb, cfg.Env); strings.Contains(out, "synthetic-secret-value") || !strings.Contains(out, "TOKEN") {
			t.Errorf("%s of Env = %s", verb, out)
		}
	}
}

// Port of test_secrets_file_allows_an_explicit_external_path.
func TestSecretsFileAllowsAnExplicitExternalPath(t *testing.T) {
	cfg := mustLoad(t, minimal+"[secrets]\nfile = \"/tmp/deployer-synthetic.env\"\n")
	if cfg.Secrets.File != "/tmp/deployer-synthetic.env" {
		t.Errorf("secrets file = %q", cfg.Secrets.File)
	}
	expectError(t, "secrets control char", minimal+"[secrets]\nfile = \"a\\tb\"\n", "control")
	expectError(t, "secrets unknown key", minimal+"[secrets]\nfile = \"a\"\nmode = 1\n", `unknown \[secrets\] setting`)
	expectError(t, "secrets without file", minimal+"[secrets]\n", `file`)
}

func TestContainerConfigurations(t *testing.T) {
	head := "[service]\nname = \"api\"\n[deploy]\nhost = \"server\"\n[container]\n"
	cfg := mustLoad(t, head+`build = "."
dockerfile = "docker/Dockerfile"
ports = ["8080:80", "127.0.0.1:9000:9000/tcp", "[::1]:53:53/udp"]
volumes = ["/srv/data:/data", "/srv/cfg:/etc/app:ro"]
args = ["--memory=512m", "--cap-drop=ALL"]
command = "serve --port 80"
`)
	c := cfg.Container
	if c.Engine != "docker" || c.Build != "." || c.Dockerfile != "docker/Dockerfile" || c.Mode() != ModeBuild || cfg.Mode() != ModeBuild {
		t.Errorf("container = %+v", c)
	}
	if len(c.Ports) != 3 || len(c.Volumes) != 2 || len(c.Args) != 2 || c.Command != "serve --port 80" {
		t.Errorf("container lists = %+v", c)
	}
	if cfg.ImageRef("release-1") != "depper/api:release-1" || cfg.ContainerName() != "depper-api" || cfg.Service.Command != "" {
		t.Errorf("names = %q %q", cfg.ImageRef("release-1"), cfg.ContainerName())
	}
	cfg = mustLoad(t, head+"engine = \"podman\"\nimage = \"ghcr.io/acme/api:1.2.3\"\n")
	if cfg.Container.Engine != "podman" || cfg.Mode() != ModeImage {
		t.Errorf("image container = %+v", cfg.Container)
	}
	cfg = mustLoad(t, head+"compose = \"deploy/compose.yaml\"\n")
	if cfg.Mode() != ModeCompose {
		t.Errorf("compose mode = %q", cfg.Mode())
	}

	bad := map[string]string{
		"with service command": "[service]\nname = \"api\"\ncommand = \"x\"\n[deploy]\nhost = \"server\"\n[container]\nimage = \"nginx\"\n",
		"none of three":        head + "engine = \"docker\"\n",
		"two of three":         head + "image = \"nginx\"\ncompose = \"c.yaml\"\n",
		"bad engine":           head + "engine = \"lxc\"\nimage = \"nginx\"\n",
		"dockerfile no build":  head + "image = \"nginx\"\ndockerfile = \"Dockerfile\"\n",
		"absolute build":       head + "build = \"/srv\"\n",
		"parent build":         head + "build = \"../other\"\n",
		"spaces compose":       head + "compose = \"my compose.yaml\"\n",
		"option-like compose":  head + "compose = \"-x.yaml\"\n",
		"bad image":            head + "image = \"nginx; rm -rf /\"\n",
		"bad port":             head + "image = \"nginx\"\nports = [\"80\"]\n",
		"port out of range":    head + "image = \"nginx\"\nports = [\"70000:80\"]\n",
		"bad port ip":          head + "image = \"nginx\"\nports = [\"localhost:80:80\"]\n",
		"bad protocol":         head + "image = \"nginx\"\nports = [\"80:80/sctp\"]\n",
		"relative volume":      head + "image = \"nginx\"\nvolumes = [\"data:/data\"]\n",
		"volume mode":          head + "image = \"nginx\"\nvolumes = [\"/a:/b:z\"]\n",
		"volume spaces":        head + "image = \"nginx\"\nvolumes = [\"/a b:/b\"]\n",
		"volume dotdot":        head + "image = \"nginx\"\nvolumes = [\"/a/../etc:/b\"]\n",
		"multiline arg":        head + "image = \"nginx\"\nargs = [\"--x\\n--y\"]\n",
		"multiline command":    head + "image = \"nginx\"\ncommand = \"a\\rb\"\n",
		"compose with ports":   head + "compose = \"c.yaml\"\nports = [\"80:80\"]\n",
		"unknown key":          head + "image = \"nginx\"\nnetwork = \"host\"\n",
		"trailing dash build":  "[service]\nname = \"api-\"\n[deploy]\nhost = \"server\"\n[container]\nbuild = \".\"\n",
	}
	for name, contents := range bad {
		expectError(t, name, contents, "")
	}
	// Build and image mode lowercase the name for the per-release image tag.
	if got := mustLoad(t, "[service]\nname = \"API\"\n[deploy]\nhost = \"server\"\n[container]\nbuild = \".\"\n").ImageRef("r1"); got != "depper/api:r1" {
		t.Errorf("ImageRef = %q", got)
	}
	if got := mustLoad(t, "[service]\nname = \"API\"\n[deploy]\nhost = \"server\"\n[container]\nimage = \"nginx\"\n").ImageRef("r1"); got != "depper/api:r1" {
		t.Errorf("image-mode ImageRef = %q", got)
	}
	expectError(t, "trailing dash image", "[service]\nname = \"api-\"\n[deploy]\nhost = \"server\"\n[container]\nimage = \"nginx\"\n", "image name")
}

func TestEndpointAndHealth(t *testing.T) {
	cfg := mustLoad(t, minimal+`[endpoint]
url = "http://203.0.113.10:8080"
[health]
url = "http://127.0.0.1:8080/health"
expect_status = 204
timeout = 2.5
start_period = 0
interval = 1
`)
	if cfg.Endpoint == nil || cfg.Endpoint.URL != "http://203.0.113.10:8080" {
		t.Errorf("endpoint = %+v", cfg.Endpoint)
	}
	h := cfg.Health
	if h.URL != "http://127.0.0.1:8080/health" || h.Command != "" || h.ExpectStatus != 204 || h.Timeout != 2500*time.Millisecond || h.StartPeriod != 0 || h.Interval != time.Second {
		t.Errorf("health = %+v", h)
	}
	cfg = mustLoad(t, minimal+"[health]\ncommand = \"test -f ready\"\n")
	if cfg.Health.Command != "test -f ready" || cfg.Health.Timeout != DefaultTimeout {
		t.Errorf("command health = %+v", cfg.Health)
	}
	if mustLoad(t, minimal).Health != nil {
		t.Error("health must be nil when unset")
	}

	bad := map[string]string{
		"endpoint scheme":      minimal + "[endpoint]\nurl = \"ftp://x\"\n",
		"endpoint creds":       minimal + "[endpoint]\nurl = \"http://u:p@x:1\"\n",
		"endpoint no host":     minimal + "[endpoint]\nurl = \"http://:8080\"\n",
		"endpoint bad port":    minimal + "[endpoint]\nurl = \"http://x:99999\"\n",
		"endpoint unknown key": minimal + "[endpoint]\nurl = \"http://x\"\nname = \"y\"\n",
		"endpoint missing url": minimal + "[endpoint]\n",
		"health both":          minimal + "[health]\nurl = \"http://x\"\ncommand = \"true\"\n",
		"health neither":       minimal + "[health]\ntimeout = 3\n",
		"health alias both":    strings.Replace(minimal, "host = \"server\"", "host = \"server\"\nhealthcheck = \"true\"", 1) + "[health]\ncommand = \"true\"\n",
		"health alias and url": strings.Replace(minimal, "host = \"server\"", "host = \"server\"\nhealthcheck = \"true\"", 1) + "[health]\nurl = \"http://x\"\n",
		"health status range":  minimal + "[health]\ncommand = \"true\"\nexpect_status = 99\n",
		"health status string": minimal + "[health]\ncommand = \"true\"\nexpect_status = \"200\"\n",
		"health zero timeout":  minimal + "[health]\ncommand = \"true\"\ntimeout = 0\n",
		"health neg period":    minimal + "[health]\ncommand = \"true\"\nstart_period = -1\n",
		"health zero interval": minimal + "[health]\ncommand = \"true\"\ninterval = 0\n",
		"health huge timeout":  minimal + "[health]\ncommand = \"true\"\ntimeout = 100000\n",
		"health bool timeout":  minimal + "[health]\ncommand = \"true\"\ntimeout = true\n",
		"health multiline":     minimal + "[health]\ncommand = \"a\\nb\"\n",
		"health bad url":       minimal + "[health]\nurl = \"127.0.0.1:8080\"\n",
		"health unknown key":   minimal + "[health]\ncommand = \"true\"\nretries = 3\n",
	}
	for name, contents := range bad {
		expectError(t, name, contents, "")
	}
}

const withTargets = `
[service]
name = "api"
command = "python app.py"

[deploy]
host = "desktop"
user = "jason"
path = "/srv/api"
setup = ["make"]
exclude = ["build/"]
healthcheck = "true"

[env]
PORT = "8080"
LOG = "info"

[secrets]
file = ".env.dev"

[endpoint]
url = "http://desktop.lan:8080"

[health]
timeout = 3

[targets.prod]
name = "api-prod"
host = "deploy@203.0.113.10"
user = "deploy"
port = 2222
identity_file = "/keys/prod"
known_hosts = "/keys/known"
path = "/srv/prod"
setup = []
keep_releases = 10
source = "git"
[targets.prod.env]
LOG = "warn"
EXTRA = "1"
[targets.prod.secrets]
file = "/secure/prod.env"
[targets.prod.endpoint]
url = "https://api.example.com"
[targets.prod.health]
url = "http://127.0.0.1:8080/health"
expect_status = 200

[targets.staging]
host = "staging.example.com"
`

func TestTargetOverlays(t *testing.T) {
	repo := repoWith(t, FileName, withTargets)
	project, err := Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(project.TargetNames(), []string{"prod", "staging"}) {
		t.Errorf("TargetNames = %q", project.TargetNames())
	}

	// prod sets host "deploy@..." and user: that conflict is reported.
	if _, err := project.ResolveWithEnv("prod", noEnv); err == nil || !strings.Contains(err.Error(), "user@host") {
		t.Errorf("prod conflict error = %v", err)
	}
	fixed := strings.Replace(withTargets, "host = \"deploy@203.0.113.10\"", "host = \"203.0.113.10\"", 1)
	project, err = Load(repoWith(t, FileName, fixed))
	if err != nil {
		t.Fatal(err)
	}
	prod, err := project.ResolveWithEnv("prod", noEnv)
	if err != nil {
		t.Fatal(err)
	}
	wantDeploy := Deploy{
		Host: "203.0.113.10", User: "deploy", Port: 2222, IdentityFile: "/keys/prod", KnownHosts: "/keys/known",
		Path: "/srv/prod", Setup: []string{}, KeepReleases: 10, Source: SourceGit,
		Exclude: append(append([]string{}, DefaultExcludes...), "build/"),
	}
	if !reflect.DeepEqual(prod.Deploy, wantDeploy) {
		t.Errorf("prod deploy = %+v\nwant %+v", prod.Deploy, wantDeploy)
	}
	if prod.Target != "prod" || prod.Service.Name != "api-prod" || prod.UnitName() != "depper-api-prod.service" {
		t.Errorf("prod service = %+v", prod.Service)
	}
	if !reflect.DeepEqual(map[string]string(prod.Env), map[string]string{"PORT": "8080", "LOG": "warn", "EXTRA": "1"}) {
		t.Error("prod env not merged over base")
	}
	if prod.Secrets.File != "/secure/prod.env" || prod.Endpoint.URL != "https://api.example.com" {
		t.Errorf("prod secrets/endpoint = %+v %+v", prod.Secrets, prod.Endpoint)
	}
	if prod.Health.URL != "http://127.0.0.1:8080/health" || prod.Health.Command != "" || prod.Health.ExpectStatus != 200 || prod.Health.Timeout != 3*time.Second {
		t.Errorf("prod health = %+v", prod.Health)
	}

	staging, err := project.ResolveWithEnv("staging", noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if staging.Deploy.Host != "staging.example.com" || staging.Deploy.User != "jason" || staging.Deploy.Path != "/srv/api" ||
		!reflect.DeepEqual(staging.Deploy.Setup, []string{"make"}) || staging.Service.Name != "api" {
		t.Errorf("staging = %+v", staging.Deploy)
	}
	if staging.Health.Command != "true" || staging.Health.Timeout != 3*time.Second {
		t.Errorf("staging health = %+v", staging.Health)
	}
	if staging.Secrets.File != filepath.Join(project.Repo, ".env.dev") {
		t.Errorf("staging secrets = %+v", staging.Secrets)
	}

	base, err := project.ResolveWithEnv("", noEnv)
	if err != nil || base.Deploy.Host != "desktop" || base.Target != "" {
		t.Errorf("base = %+v, %v", base, err)
	}

	// Resolve returns independent copies.
	staging.Env["LOG"] = "mutated"
	staging.Deploy.Setup[0] = "mutated"
	again, _ := project.ResolveWithEnv("staging", noEnv)
	if again.Env["LOG"] != "info" || again.Deploy.Setup[0] != "make" {
		t.Error("Resolve results share state")
	}

	if _, err := project.ResolveWithEnv("nope", noEnv); err == nil || !strings.Contains(err.Error(), "available: prod, staging") {
		t.Errorf("unknown target error = %v", err)
	}
}

func TestTargetsWithoutBaseHost(t *testing.T) {
	repo := repoWith(t, FileName, `
[service]
name = "api"
command = "x"
[deploy]
path = "/srv/api"
[targets.b]
host = "b.example"
[targets.a]
host = "a.example"
[targets.c]
port = 22
`)
	project, err := Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := project.ResolveWithEnv("", noEnv); err == nil || !strings.Contains(err.Error(), "no target selected") || !strings.Contains(err.Error(), "a, b, c") {
		t.Errorf("no-target error = %v", err)
	}
	if _, err := project.ResolveWithEnv("c", noEnv); err == nil || !strings.Contains(err.Error(), "host is required") {
		t.Errorf("target without host error = %v", err)
	}
	if cfg, err := project.ResolveWithEnv("a", noEnv); err != nil || cfg.Deploy.Host != "a.example" {
		t.Errorf("target a = %v %v", cfg, err)
	}
	if _, err := LoadTarget(repoWith(t, FileName, minimal), "prod", noEnv); err == nil || !strings.Contains(err.Error(), "defines no [targets]") {
		t.Errorf("unknown target without targets = %v", err)
	}
}

func TestTargetHealthOverlayRules(t *testing.T) {
	base := minimal + "[health]\nurl = \"http://127.0.0.1:1/h\"\n"
	project, err := Load(repoWith(t, FileName, base+"[targets.x]\nhost = \"x\"\n[targets.x.health]\ncommand = \"true\"\n[targets.y]\nhost = \"y\"\n[targets.y.health]\ninterval = 5\n"))
	if err != nil {
		t.Fatal(err)
	}
	x, err := project.ResolveWithEnv("x", noEnv)
	if err != nil || x.Health.Command != "true" || x.Health.URL != "" {
		t.Errorf("x health = %+v %v", x, err)
	}
	y, err := project.ResolveWithEnv("y", noEnv)
	if err != nil || y.Health.URL != "http://127.0.0.1:1/h" || y.Health.Interval != 5*time.Second {
		t.Errorf("y health = %+v %v", y, err)
	}
	project, err = Load(repoWith(t, FileName, minimal+"[targets.z]\nhost = \"z\"\n[targets.z.health]\ntimeout = 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := project.ResolveWithEnv("z", noEnv); err == nil || !strings.Contains(err.Error(), "exactly one of url or command") {
		t.Errorf("z error = %v", err)
	}
}

func TestInvalidTargets(t *testing.T) {
	bad := map[string]string{
		"bad target name":       minimal + "[targets.\"a b\"]\nhost = \"x\"\n",
		"target not table":      minimal + "[targets]\nprod = \"x\"\n",
		"unknown target key":    minimal + "[targets.p]\nexclude = [\"x\"]\n",
		"target container":      minimal + "[targets.p]\n[targets.p.container]\nimage = \"x\"\n",
		"target bad host":       minimal + "[targets.p]\nhost = \"-oProxyCommand=x\"\n",
		"target bad port":       minimal + "[targets.p]\nport = 0\n",
		"target bad name":       minimal + "[targets.p]\nname = \"a/b\"\n",
		"target bad env":        minimal + "[targets.p]\n[targets.p.env]\n\"A-B\" = \"x\"\n",
		"target env multiline":  minimal + "[targets.p]\n[targets.p.env]\nA = \"x\\ny\"\n",
		"target secrets key":    minimal + "[targets.p]\n[targets.p.secrets]\npath = \"x\"\n",
		"target endpoint url":   minimal + "[targets.p]\n[targets.p.endpoint]\nurl = \"x\"\n",
		"target health both":    minimal + "[targets.p]\n[targets.p.health]\nurl = \"http://x\"\ncommand = \"true\"\n",
		"target health key":     minimal + "[targets.p]\n[targets.p.health]\nhealthcheck = \"true\"\n",
		"target keep releases":  minimal + "[targets.p]\nkeep_releases = 1\n",
		"target source":         minimal + "[targets.p]\nsource = \"svn\"\n",
		"target path":           minimal + "[targets.p]\npath = \"relative\"\n",
		"target setup newline":  minimal + "[targets.p]\nsetup = [\"a\\nb\"]\n",
		"target env not table":  minimal + "[targets.p]\nenv = \"x\"\n",
		"target known_hosts ws": minimal + "[targets.p]\nknown_hosts = \"\"\n",
	}
	for name, contents := range bad {
		_, err := Load(repoWith(t, FileName, contents))
		if err == nil {
			t.Errorf("%s: expected load error", name)
		}
	}
}

func TestNameHelpers(t *testing.T) {
	cfg := mustLoad(t, minimal)
	if cfg.LegacyUnitName() != "deployer-api.service" || cfg.LegacyTunnelUnitName() != "deployer-tunnel@api.service" {
		t.Error("legacy names")
	}
	if cfg.DefaultBaseRel() != ".local/share/depper/api" || cfg.LegacyDefaultBaseRel() != ".local/share/deployer/api" {
		t.Error("base paths")
	}
}

// ssh splits UserKnownHostsFile at whitespace, so paths with whitespace are
// refused for both keys and both CI overrides.
func TestSSHFilePathsRejectWhitespace(t *testing.T) {
	expectError(t, "identity with space", minimal+"identity_file = \"/keys/my key\"\n", `identity_file .*whitespace`)
	expectError(t, "known_hosts with space", minimal+"known_hosts = \"/keys/known hosts\"\n", `known_hosts .*whitespace`)
	expectError(t, "known_hosts with tab", minimal+"known_hosts = \"/keys/known\\thosts\"\n", `known_hosts`)
	repo := repoWith(t, FileName, minimal)
	for _, name := range []string{EnvSSHKeyFile, EnvKnownHostsKey} {
		_, err := LoadTarget(repo, "", func(n string) (string, bool) { return "/ci/my file", n == name })
		if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "whitespace") {
			t.Errorf("%s with a space: %v", name, err)
		}
	}
}

// A trailing slash on a deployment path is refused at load time for the base
// [deploy] table and for [targets.<name>] overlays alike.
func TestDeployPathTrailingSlash(t *testing.T) {
	cases := []struct {
		name     string
		contents string
	}{
		{"base deploy path", minimal + "path = \"/srv/app/\"\n"},
		{"target path", minimal + "[targets.prod]\npath = \"/srv/app/\"\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expectError(t, tc.name, tc.contents, `\.path must not end with /`)
		})
	}
	if cfg := mustLoad(t, minimal+"path = \"/srv/app\"\n"); cfg.Deploy.Path != "/srv/app" {
		t.Errorf("path = %q", cfg.Deploy.Path)
	}
}

// [deploy].path is used unquoted (rsync destination, unit paths), so only a
// conservative alphabet is accepted.
func TestDeployPathAlphabet(t *testing.T) {
	for _, bad := range []string{"/srv/my app", "/srv/a:b", "/srv/a$b", "/srv/ü", "/", "relative"} {
		expectError(t, "path "+bad, minimal+"path = \""+bad+"\"\n", "path must be an absolute remote path")
	}
	for _, good := range []string{"/srv/api", "/srv/a+b@c_d.e-f"} {
		if cfg := mustLoad(t, minimal+"path = \""+good+"\"\n"); cfg.Deploy.Path != good {
			t.Errorf("path = %q", cfg.Deploy.Path)
		}
	}
}
