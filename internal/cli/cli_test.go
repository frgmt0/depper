package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/frgmt0/depper/internal/config"
	"github.com/frgmt0/depper/internal/exitcode"
	"github.com/frgmt0/depper/internal/transport/transporttest"
)

func testApp(env map[string]string) (*App, *bytes.Buffer, *bytes.Buffer) {
	var stdout, stderr bytes.Buffer
	app := &App{
		Stdin:  strings.NewReader(""),
		Stdout: &stdout,
		Stderr: &stderr,
		LookupEnv: func(name string) (string, bool) {
			v, ok := env[name]
			return v, ok
		},
		Exec: &transporttest.Recorder{},
	}
	return app, &stdout, &stderr
}

func tempRepo(t *testing.T, name string) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(dir, name)
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestInitWritesAConfigThatLoads(t *testing.T) {
	repo := tempRepo(t, "My Repo!")
	app, stdout, _ := testApp(nil)
	if code := Run(context.Background(), app, []string{"init", repo}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	path := filepath.Join(repo, config.FileName)
	if stdout.String() != "Created "+path+"\n" {
		t.Errorf("stdout = %q", stdout.String())
	}
	cfg, err := config.LoadTarget(repo, "", func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatalf("written config does not load: %v", err)
	}
	if cfg.Service.Name != "My-Repo-" || cfg.Deploy.Host != "deploy@203.0.113.10" || cfg.Service.Command == "" {
		t.Errorf("cfg = %+v host %q", cfg.Service, cfg.Deploy.Host)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(written), "desktop") {
		t.Errorf("the starter template contains a personal host alias:\n%s", written)
	}

	app, _, stderr := testApp(nil)
	if code := Run(context.Background(), app, []string{"init", repo}); code != exitcode.Failure {
		t.Errorf("second init exit = %d", code)
	}
	if !strings.HasPrefix(stderr.String(), "depper: ") || !strings.Contains(stderr.String(), "already exists") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestInitRefusesLegacyConfigAndMissingDirectory(t *testing.T) {
	repo := tempRepo(t, "legacy")
	if err := os.WriteFile(filepath.Join(repo, config.LegacyFileName), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := writeStarterConfig(repo); err == nil || !strings.Contains(err.Error(), "deploy.toml already exists") {
		t.Errorf("legacy error = %v", err)
	}
	if _, err := writeStarterConfig(filepath.Join(repo, "missing")); err == nil {
		t.Error("missing directory accepted")
	}
}

func TestInitJSONOutput(t *testing.T) {
	repo := tempRepo(t, "api")
	app, stdout, _ := testApp(nil)
	if code := Run(context.Background(), app, []string{"--json", "init", repo}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var out map[string]string
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil || out["created"] != filepath.Join(repo, config.FileName) {
		t.Errorf("json = %q (%v)", stdout.String(), err)
	}
}

// Every commented example in the template must be valid once uncommented
// (the container block replaces [service].command and the health command
// line is the alternative to url, so those are checked separately).
func TestStarterTemplateExamplesAreValid(t *testing.T) {
	var lines []string
	inContainer := false
	for _, line := range strings.Split(starterConfig("api"), "\n") {
		body, commented := strings.CutPrefix(line, "# ")
		if commented && strings.HasPrefix(body, "[") {
			inContainer = strings.HasPrefix(body, "[container]")
		}
		// [deploy].user is the alternative to the user@ prefix in host, so
		// the two cannot both be active.
		if inContainer || strings.HasPrefix(body, `command = "curl`) || strings.HasPrefix(body, `user = `) {
			continue
		}
		if commented && (strings.HasPrefix(body, "[") || strings.Contains(body, " = ")) {
			line = body
		}
		lines = append(lines, line)
	}
	repo := tempRepo(t, "api")
	if err := os.WriteFile(filepath.Join(repo, config.FileName), []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	project, err := config.Load(repo)
	if err != nil {
		t.Fatalf("uncommented template invalid: %v\n%s", err, strings.Join(lines, "\n"))
	}
	for _, target := range []string{"", "prod"} {
		cfg, err := project.ResolveWithEnv(target, func(string) (string, bool) { return "", false })
		if err != nil {
			t.Fatalf("resolve %q: %v", target, err)
		}
		if cfg.Health == nil || cfg.Endpoint == nil || cfg.Secrets == nil || cfg.Tunnel == nil {
			t.Errorf("examples missing: %+v", cfg)
		}
	}

	// Container example: uncomment it and drop [service].command.
	var container []string
	inContainer = false
	for _, line := range strings.Split(starterConfig("api"), "\n") {
		body, commented := strings.CutPrefix(line, "# ")
		if commented && strings.HasPrefix(body, "[") {
			inContainer = strings.HasPrefix(body, "[container]")
		}
		if strings.HasPrefix(line, "command = ") {
			continue
		}
		if inContainer && commented && (strings.HasPrefix(body, "[") || strings.Contains(body, " = ")) {
			line = body
		}
		container = append(container, line)
	}
	if err := os.WriteFile(filepath.Join(repo, config.FileName), []byte(strings.Join(container, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadTarget(repo, "", func(string) (string, bool) { return "", false })
	if err != nil || cfg.Mode() != config.ModeBuild {
		t.Errorf("container example: %v", err)
	}
}

func TestServiceNameFor(t *testing.T) {
	cases := map[string]string{
		"/x/api":                        "api",
		"/x/my app":                     "my-app",
		"/x/.hidden":                    "hidden",
		"/x/---":                        "app",
		"/x/" + strings.Repeat("a", 80): strings.Repeat("a", 63),
	}
	for in, want := range cases {
		if got := serviceNameFor(in); got != want {
			t.Errorf("serviceNameFor(%q) = %q, want %q", in, got, want)
		}
	}
}

func fakeRoot() *cobra.Command {
	root := &cobra.Command{Use: "depper"}
	for _, name := range []string{"deploy", "status", "logs", "init"} {
		root.AddCommand(&cobra.Command{Use: name})
	}
	return root
}

func TestRewriteArgs(t *testing.T) {
	cases := []struct{ in, want []string }{
		{nil, []string{"deploy"}},
		{[]string{"--json"}, []string{"deploy", "--json"}},
		{[]string{"--target", "prod"}, []string{"deploy", "--target", "prod"}},
		{[]string{"/repo"}, []string{"deploy", "/repo"}},
		{[]string{"--dry-run", "/repo"}, []string{"deploy", "--dry-run", "/repo"}},
		{[]string{"/repo", "deploy", "--dry-run"}, []string{"deploy", "/repo", "--dry-run"}},
		{[]string{"/repo", "status"}, []string{"status", "/repo"}},
		{[]string{"--target", "prod", "/repo", "logs", "--lines", "5"}, []string{"logs", "--target", "prod", "/repo", "--lines", "5"}},
		{[]string{"/repo", "init"}, []string{"deploy", "/repo", "init"}},
		{[]string{"status", "/repo"}, []string{"status", "/repo"}},
		{[]string{"logs", "/repo", "--lines", "25"}, []string{"logs", "/repo", "--lines", "25"}},
		{[]string{"init"}, []string{"init"}},
		{[]string{"--help"}, []string{"--help"}},
		{[]string{"-h"}, []string{"-h"}},
		{[]string{"--version"}, []string{"--version"}},
		{[]string{"help", "status"}, []string{"help", "status"}},
		{[]string{"--target"}, []string{"--target"}},
		{[]string{"--", "x"}, []string{"--", "x"}},
	}
	root := fakeRoot()
	for _, tc := range cases {
		if got := rewriteArgs(root, tc.in); !reflect.DeepEqual(got, tc.want) && !(len(got) == 0 && len(tc.want) == 0) {
			t.Errorf("rewriteArgs(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRewriteArgsIsInactiveWithoutDeploy(t *testing.T) {
	root := &cobra.Command{Use: "depper"}
	root.AddCommand(&cobra.Command{Use: "init"})
	if got := rewriteArgs(root, []string{"/repo"}); !reflect.DeepEqual(got, []string{"/repo"}) {
		t.Errorf("got %q", got)
	}
}

func TestTargetSelection(t *testing.T) {
	app, _, _ := testApp(map[string]string{EnvTarget: "staging"})
	if app.Target() != "staging" {
		t.Errorf("env target = %q", app.Target())
	}
	root := NewRootCommand(app)
	if err := root.ParseFlags([]string{"--target", "prod"}); err != nil {
		t.Fatal(err)
	}
	if app.Target() != "prod" {
		t.Errorf("flag target = %q", app.Target())
	}
}

func TestAppConfigUsesSelectedTargetAndEnv(t *testing.T) {
	repo := tempRepo(t, "api")
	contents := "[service]\nname = \"api\"\ncommand = \"x\"\n[deploy]\nhost = \"base\"\n[targets.prod]\nhost = \"prod.example\"\n"
	if err := os.WriteFile(filepath.Join(repo, config.FileName), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	app, _, _ := testApp(map[string]string{EnvTarget: "prod", config.EnvSSHKeyFile: "/ci/key"})
	cfg, err := app.Config(repo)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Target != "prod" || cfg.Deploy.Host != "prod.example" || cfg.Deploy.IdentityFile != "/ci/key" {
		t.Errorf("cfg = %+v", cfg.Deploy)
	}
	remote := app.Remote(cfg, false)
	if remote.Target.Host != "prod.example" || remote.Exec != app.Exec {
		t.Errorf("remote = %+v", remote)
	}
}

func TestRunMapsExitCodesAndPrefixesErrors(t *testing.T) {
	saved := factories
	t.Cleanup(func() { factories = saved })
	register(func(*App) *cobra.Command {
		return &cobra.Command{Use: "boom", RunE: func(*cobra.Command, []string) error {
			return exitcode.New(exitcode.Drift, errors.New("outdated"))
		}}
	})
	register(func(*App) *cobra.Command {
		return &cobra.Command{Use: "quiet", RunE: func(*cobra.Command, []string) error {
			return exitcode.Silent(exitcode.Unhealthy)
		}}
	})
	app, _, stderr := testApp(nil)
	if code := Run(context.Background(), app, []string{"boom"}); code != exitcode.Drift || stderr.String() != "depper: outdated\n" {
		t.Errorf("boom: code %d stderr %q", code, stderr.String())
	}
	app, _, stderr = testApp(nil)
	if code := Run(context.Background(), app, []string{"quiet"}); code != exitcode.Unhealthy || stderr.String() != "" {
		t.Errorf("quiet: code %d stderr %q", code, stderr.String())
	}
	app, _, stderr = testApp(nil)
	if code := Run(context.Background(), app, []string{"no-such-command"}); code != exitcode.Failure || !strings.HasPrefix(stderr.String(), "depper: ") {
		t.Errorf("unknown: code %d stderr %q", code, stderr.String())
	}
}

func TestHumanOutputGoesToStderrInJSONMode(t *testing.T) {
	app, stdout, stderr := testApp(nil)
	app.Printf("hello\n")
	app.JSON = true
	app.Printf("note\n")
	if stdout.String() != "hello\n" || stderr.String() != "note\n" {
		t.Errorf("stdout %q stderr %q", stdout.String(), stderr.String())
	}
}
