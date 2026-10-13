package cli

import (
	"bytes"
	"context"
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
