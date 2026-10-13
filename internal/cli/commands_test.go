package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/frgmt0/depper/internal/config"
	"github.com/frgmt0/depper/internal/exitcode"
	"github.com/frgmt0/depper/internal/transport"
	"github.com/frgmt0/depper/internal/transport/transporttest"
)

const cliSentinel = "synthetic-secret-not-for-output"

func repoWith(t *testing.T, toml string, files map[string]string) string {
	t.Helper()
	repo := tempRepo(t, "api")
	if err := os.WriteFile(filepath.Join(repo, config.FileName), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

const cliToml = "[service]\nname = \"api\"\ncommand = \"python app.py\"\n[deploy]\nhost = \"server\"\n[secrets]\nfile = \"missing.env\"\n"

// fakeRemote answers the scripts depper sends, keyed by recognisable lines.
func fakeRemote(layout string, extra func(transporttest.Call) (transport.Result, error)) func(transporttest.Call) (transport.Result, error) {
	return func(c transporttest.Call) (transport.Result, error) {
		if c.Program() == "git" {
			return transport.Result{}, &transport.CommandError{Program: "git", ExitCode: 128, Stderr: "not a git repository"}
		}
		if strings.Contains(c.Stdin, `printf 'layout=%s\n'`) {
			return transport.Result{Stdout: "base=/home/u/.local/share/depper/api\nhome=/home/u\nlayout=" + layout + "\ntunnel=no\n"}, nil
		}
		if extra != nil {
			return extra(c)
		}
		return transport.Result{}, nil
	}
}

func recorderOf(app *App) *transporttest.Recorder { return app.Exec.(*transporttest.Recorder) }

func TestDeployDryRunForms(t *testing.T) {
	repo := repoWith(t, strings.Replace(cliToml, "missing.env", ".env.prod", 1), map[string]string{".env.prod": "TOKEN=" + cliSentinel + "\n"})
	for _, args := range [][]string{{repo, "deploy", "--dry-run"}, {"deploy", "--dry-run", repo}, {"--dry-run", repo}} {
		app, stdout, stderr := testApp(nil)
		recorderOf(app).Handler = fakeRemote("depper", nil)
		if code := Run(context.Background(), app, args); code != 0 {
			t.Fatalf("%q: exit %d: %s", args, code, stderr.String())
		}
		for _, c := range recorderOf(app).Calls() {
			if c.Program() != "git" {
				t.Errorf("%q: dry run ran %q", args, c.Args)
			}
		}
		if !strings.Contains(stdout.String(), "DRY RUN: deploy api to server:~/.local/share/depper/api") || strings.Contains(stdout.String(), cliSentinel) {
			t.Errorf("%q: output %q", args, stdout.String())
		}
	}
}

func TestDeployRejectsEnvAndEnvStdinTogetherAndMissingSecrets(t *testing.T) {
	repo := repoWith(t, cliToml, nil)
	app, _, stderr := testApp(nil)
	if code := Run(context.Background(), app, []string{"deploy", repo, "--env", "x.env", "--env-stdin"}); code != exitcode.Failure || !strings.Contains(stderr.String(), "cannot be combined") {
		t.Errorf("exit %d stderr %q", code, stderr.String())
	}
	app, _, stderr = testApp(nil)
	code := Run(context.Background(), app, []string{"deploy", repo})
	if code != exitcode.Failure || !strings.Contains(stderr.String(), "not readable") || strings.Contains(stderr.String(), "missing.env") {
		t.Errorf("exit %d stderr %q", code, stderr.String())
	}
	if len(recorderOf(app).Calls()) != 0 {
		t.Error("remote commands ran before the dotenv file was validated")
	}
}

func TestNonDeployCommandsDoNotReadTheEnvFile(t *testing.T) {
	repo := repoWith(t, cliToml, nil)
	app, stdout, stderr := testApp(nil)
	recorderOf(app).Handler = fakeRemote("depper", func(c transporttest.Call) (transport.Result, error) {
		if strings.Contains(c.Stdin, "echo @@unit") {
			return transport.Result{Stdout: "@@unit\nActiveState=active\nSubState=running\nMainPID=10\nNRestarts=0\nActiveEnterTimestamp=Mon\n@@current\nreleases/release-20260101T000000Z-00000001\n@@previous\n@@release\n"}, nil
		}
		if strings.Contains(c.Stdin, "systemctl --user show") {
			return transport.Result{Stdout: "ActiveState=active\nSubState=running\n"}, nil
		}
		return transport.Result{}, nil
	})
	if code := Run(context.Background(), app, []string{"--json", "status", repo}); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var status map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
		t.Fatalf("status json: %v\n%s", err, stdout.String())
	}
	var keys []string
	for k := range status {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := "active_enter_timestamp,active_state,base,current_release,deployed,endpoint_url,health,health_error,host,load_state,main_pid,n_restarts,previous_release,release,service,sub_state,target,tunnel_url,unit"
	if strings.Join(keys, ",") != want {
		t.Errorf("status keys = %s", strings.Join(keys, ","))
	}
	if status["active_state"] != "active" || status["current_release"] != "release-20260101T000000Z-00000001" {
		t.Errorf("status = %v", status)
	}
}

func TestLogsPassesLineCountAndFollow(t *testing.T) {
	repo := repoWith(t, cliToml, nil)
	app, stdout, _ := testApp(nil)
	recorderOf(app).Handler = fakeRemote("depper", func(c transporttest.Call) (transport.Result, error) {
		return transport.Result{Stdout: "log line\n"}, nil
	})
	if code := Run(context.Background(), app, []string{"logs", repo, "--lines", "25", "--follow"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	scripts := recorderOf(app).Scripts()
	if !strings.Contains(scripts[len(scripts)-1], "journalctl --user-unit depper-api.service --no-pager -n 25 -f") || stdout.String() != "log line\n" {
		t.Errorf("scripts %q stdout %q", scripts, stdout.String())
	}
	app, _, stderr := testApp(nil)
	if code := Run(context.Background(), app, []string{"logs", repo, "--lines", "0"}); code != exitcode.Failure || !strings.Contains(stderr.String(), "positive") {
		t.Errorf("--lines 0: exit %d %q", code, stderr.String())
	}
}

func TestHealthExitCodes(t *testing.T) {
	repo := repoWith(t, cliToml+"[health]\ncommand = \"./check\"\n", nil)
	for _, tc := range []struct {
		fail bool
		code int
	}{{false, 0}, {true, exitcode.Unhealthy}} {
		app, stdout, stderr := testApp(nil)
		recorderOf(app).Handler = fakeRemote("depper", func(c transporttest.Call) (transport.Result, error) {
			if strings.Contains(c.Stdin, "/bin/sh -lc ./check") {
				if tc.fail {
					return transport.Result{Stdout: `{"state":"starting"}`}, &transport.CommandError{Program: "ssh", ExitCode: 1, Stderr: "not ready"}
				}
				return transport.Result{Stdout: `{"state":"running","progress":0.4}`}, nil
			}
			return transport.Result{}, nil
		})
		code := Run(context.Background(), app, []string{"health", repo, "--json"})
		if code != tc.code {
			t.Fatalf("fail=%v: exit %d (%s)", tc.fail, code, stderr.String())
		}
		var res map[string]any
		if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		if res["ok"] != !tc.fail || res["check"] != "command" {
			t.Errorf("result = %v", res)
		}
		if !tc.fail && res["body"].(map[string]any)["progress"] != 0.4 {
			t.Errorf("body = %v", res["body"])
		}
	}
	// --watch with --count emits one JSON line per check.
	app, stdout, _ := testApp(nil)
	recorderOf(app).Handler = fakeRemote("depper", nil)
	if code := Run(context.Background(), app, []string{"health", repo, "--json", "--watch", "0.01", "--count", "3"}); code != 0 {
		t.Fatalf("watch exit %d", code)
	}
	if lines := strings.Split(strings.TrimSpace(stdout.String()), "\n"); len(lines) != 3 {
		t.Errorf("watch output = %q", stdout.String())
	}
}

func TestCheckExitsThreeWhenNotDeployed(t *testing.T) {
	repo := repoWith(t, strings.Replace(cliToml, "[secrets]\nfile = \"missing.env\"\n", "", 1), nil)
	app, stdout, stderr := testApp(nil)
	recorderOf(app).Handler = fakeRemote("none", nil)
	if code := Run(context.Background(), app, []string{"check", repo, "--json"}); code != exitcode.Drift {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var res map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil || res["state"] != "not-deployed" || res["drift"] != true {
		t.Errorf("check = %s (%v)", stdout.String(), err)
	}
}

func TestUnlockAndControlCommands(t *testing.T) {
	repo := repoWith(t, cliToml, nil)
	app, stdout, _ := testApp(nil)
	recorderOf(app).Handler = fakeRemote("depper", func(c transporttest.Call) (transport.Result, error) {
		if strings.Contains(c.Stdin, "echo free") {
			return transport.Result{Stdout: "free\n"}, nil
		}
		return transport.Result{}, nil
	})
	if code := Run(context.Background(), app, []string{"unlock", repo}); code != 0 || stdout.String() != "No lock is held.\n" {
		t.Errorf("unlock: exit %d %q", code, stdout.String())
	}
	for _, action := range []string{"restart", "stop"} {
		app, stdout, _ := testApp(nil)
		recorderOf(app).Handler = fakeRemote("depper", nil)
		if code := Run(context.Background(), app, []string{action, repo}); code != 0 {
			t.Fatalf("%s exit %d", action, code)
		}
		scripts := recorderOf(app).Scripts()
		if !strings.Contains(scripts[len(scripts)-1], "systemctl --user "+action+" depper-api.service") || !strings.Contains(stdout.String(), "depper-api.service") {
			t.Errorf("%s: %q %q", action, scripts, stdout.String())
		}
	}
	app, _, stderr := testApp(nil)
	recorderOf(app).Handler = fakeRemote("legacy", nil)
	if code := Run(context.Background(), app, []string{"restart", repo}); code != exitcode.Failure || !strings.Contains(stderr.String(), "depper migrate") {
		t.Errorf("legacy restart: exit %d %q", code, stderr.String())
	}
}

func TestRollbackRejectsInvalidReleaseID(t *testing.T) {
	repo := repoWith(t, cliToml, nil)
	app, _, stderr := testApp(nil)
	if code := Run(context.Background(), app, []string{"rollback", repo, "--to", "../../etc"}); code != exitcode.Failure || !strings.Contains(stderr.String(), "invalid release id") {
		t.Errorf("exit %d %q", code, stderr.String())
	}
	if len(recorderOf(app).Calls()) != 0 {
		t.Error("remote contacted for an invalid release id")
	}
}
