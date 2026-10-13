package cli

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/frgmt0/depper/internal/engine"
	"github.com/frgmt0/depper/internal/exitcode"
	"github.com/frgmt0/depper/internal/transport"
	"github.com/frgmt0/depper/internal/transport/transporttest"
)

const (
	secretsTestRelease = "release-20260101T000000Z-00000001"
	secretsToml        = cliToml + "[health]\ncommand = \"./check\"\nstart_period = 1\ninterval = 1\n"
)

var secretsSalt = []byte("0123456789abcdef0123456789abcdef")

// hostEnv describes the secrets state of the fake host.
type hostEnv struct {
	manifest *engine.Manifest // nil: no manifest or salt yet
	liveSHA  string           // sha256 of the live file; "" means the file is missing
}

// hostWith records the manifest depper would have written for values.
func hostWith(values map[string]string, body string) hostEnv {
	m := engine.NewManifest(secretsSalt, []byte(body), values, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	return hostEnv{manifest: m, liveSHA: engine.SHA256Hex([]byte(body))}
}

func (h hostEnv) stateOutput() string {
	out := "@@salt\n"
	if h.manifest != nil {
		out += hex.EncodeToString(secretsSalt) + "\n"
	}
	out += "@@manifest\n"
	if h.manifest != nil {
		out += string(h.manifest.Encode())
	}
	out += "@@live\n"
	if h.liveSHA != "" {
		out += h.liveSHA + "  -\n"
	}
	return out
}

// secretsRemote answers the scripts of secrets list/diff/push.
func secretsRemote(host hostEnv, healthy bool) func(transporttest.Call) (transport.Result, error) {
	return fakeRemote("depper", func(c transporttest.Call) (transport.Result, error) {
		switch {
		case strings.Contains(c.Stdin, "echo @@salt"):
			return transport.Result{Stdout: host.stateOutput()}, nil
		case strings.Contains(c.Stdin, `readlink "$base/current"`):
			return transport.Result{Stdout: "releases/" + secretsTestRelease + "\n"}, nil
		case strings.Contains(c.Stdin, "echo acquired"):
			return transport.Result{Stdout: "acquired\n"}, nil
		case strings.Contains(c.Stdin, "systemctl --user show"):
			return transport.Result{Stdout: "ActiveState=active\nSubState=running\nNRestarts=0\n"}, nil
		case strings.Contains(c.Stdin, "/bin/sh -lc ./check"):
			if !healthy {
				return transport.Result{Stdout: `{"state":"starting"}`}, &transport.CommandError{Program: "ssh", ExitCode: 1, Stderr: "not ready"}
			}
			return transport.Result{Stdout: `{"state":"running"}`}, nil
		}
		return transport.Result{}, nil
	})
}

func secretsRepo(t *testing.T) string {
	t.Helper()
	return repoWith(t, secretsToml, map[string]string{"missing.env": "TOKEN=" + cliSentinel + "\nKEEP=same-value\n"})
}

func runSecrets(t *testing.T, handler func(transporttest.Call) (transport.Result, error), stdin string, args ...string) (code int, stdout, stderr string, app *App) {
	t.Helper()
	app, out, errOut := testApp(nil)
	app.Stdin = strings.NewReader(stdin)
	recorderOf(app).Handler = handler
	code = Run(context.Background(), app, args)
	return code, out.String(), errOut.String(), app
}

func assertNoSecrets(t *testing.T, app *App, outputs ...string) {
	t.Helper()
	for _, o := range outputs {
		for _, bad := range []string{cliSentinel, "same-value", hex.EncodeToString(secretsSalt)} {
			if strings.Contains(o, bad) {
				t.Errorf("output leaks %q: %q", bad, o)
			}
		}
	}
	for _, c := range recorderOf(app).Calls() {
		for _, a := range c.Args {
			if strings.Contains(a, cliSentinel) {
				t.Errorf("secret value in argv: %q", c.Args)
			}
		}
	}
}

func TestSecretsListShowsNamesAndWarnsOnOutsideEdit(t *testing.T) {
	repo := secretsRepo(t)
	values := map[string]string{"TOKEN": cliSentinel, "KEEP": "same-value"}
	for _, modified := range []bool{false, true} {
		host := hostWith(values, "TOKEN="+cliSentinel+"\nKEEP=same-value\n")
		if modified {
			host.liveSHA = engine.SHA256Hex([]byte("edited by hand"))
		}
		code, stdout, stderr, app := runSecrets(t, secretsRemote(host, true), "", "secrets", "list", repo)
		if code != 0 {
			t.Fatalf("exit %d: %s", code, stderr)
		}
		for _, want := range []string{"2 variables", "2026-01-02T03:04:05Z", "  KEEP\n", "  TOKEN\n"} {
			if !strings.Contains(stdout, want) {
				t.Errorf("modified=%v: stdout missing %q: %q", modified, want, stdout)
			}
		}
		if got := strings.Contains(stdout, "modified outside depper"); got != modified {
			t.Errorf("modified=%v: warning present = %v: %q", modified, got, stdout)
		}
		for _, fp := range host.manifest.Keys {
			if strings.Contains(stdout, fp) || strings.Contains(stderr, fp) {
				t.Errorf("fingerprint printed: %q", stdout)
			}
		}
		assertNoSecrets(t, app, stdout, stderr)
	}
}

func TestSecretsListJSONAndNoManifest(t *testing.T) {
	repo := secretsRepo(t)
	host := hostWith(map[string]string{"TOKEN": cliSentinel}, "TOKEN="+cliSentinel+"\n")
	code, stdout, stderr, app := runSecrets(t, secretsRemote(host, true), "", "--json", "secrets", "list", repo)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("json: %v\n%s", err, stdout)
	}
	if res["known"] != true || res["updated_at"] != "2026-01-02T03:04:05Z" || res["file_modified"] != false || res["service"] != "api" {
		t.Errorf("result = %v", res)
	}
	if keys, _ := res["keys"].([]any); len(keys) != 1 || keys[0] != "TOKEN" {
		t.Errorf("keys = %v", res["keys"])
	}
	assertNoSecrets(t, app, stdout, stderr)
	for _, fp := range host.manifest.Keys {
		if strings.Contains(stdout, fp) {
			t.Error("fingerprint in JSON")
		}
	}

	code, stdout, _, _ = runSecrets(t, secretsRemote(hostEnv{}, true), "", "secrets", "list", repo)
	if code != 0 || !strings.Contains(stdout, "no environment manifest") {
		t.Errorf("no manifest: exit %d %q", code, stdout)
	}
}

func TestSecretsDiffInSyncAndDrift(t *testing.T) {
	repo := secretsRepo(t)
	deployed := map[string]string{"TOKEN": cliSentinel, "KEEP": "same-value", "OLD": "gone-value"}
	host := hostWith(deployed, "deployed body")

	// Local file has TOKEN changed, OLD removed, NEW added, KEEP same.
	local := "TOKEN=changed-" + cliSentinel + "\nKEEP=same-value\nNEW=new-value\n"
	code, stdout, stderr, app := runSecrets(t, secretsRemote(host, true), local, "secrets", "diff", repo, "--env-stdin")
	if code != exitcode.Drift {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	for _, want := range []string{"drift", "added    NEW", "removed  OLD", "changed  TOKEN", "1 unchanged"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q: %q", want, stdout)
		}
	}
	if strings.Contains(stdout, "KEEP") {
		t.Errorf("unchanged key listed as a difference: %q", stdout)
	}
	assertNoSecrets(t, app, stdout, stderr)
	if strings.Contains(stdout, "new-value") || strings.Contains(stdout, "gone-value") {
		t.Errorf("value in output: %q", stdout)
	}

	code, stdout, stderr, _ = runSecrets(t, secretsRemote(host, true), local, "--json", "secrets", "diff", repo, "--env-stdin")
	if code != exitcode.Drift {
		t.Fatalf("json exit %d: %s", code, stderr)
	}
	var res struct {
		State string `json:"state"`
		Keys  struct {
			Added, Removed, Changed, Unchanged []string
		} `json:"keys"`
		Service string `json:"service"`
	}
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("json: %v\n%s", err, stdout)
	}
	join := strings.Join
	if res.State != "drift" || res.Service != "api" || join(res.Keys.Added, ",") != "NEW" || join(res.Keys.Removed, ",") != "OLD" ||
		join(res.Keys.Changed, ",") != "TOKEN" || join(res.Keys.Unchanged, ",") != "KEEP" {
		t.Errorf("diff = %+v", res)
	}
	if strings.Contains(stdout, "new-value") || strings.Contains(stdout, "gone-value") {
		t.Errorf("value in JSON: %s", stdout)
	}

	// In sync: exit 0.
	same := map[string]string{"TOKEN": cliSentinel, "KEEP": "same-value"}
	body := "TOKEN=" + cliSentinel + "\nKEEP=same-value\n"
	code, stdout, stderr, app = runSecrets(t, secretsRemote(hostWith(same, body), true), "", "secrets", "diff", repo)
	if code != 0 || !strings.Contains(stdout, "in-sync") {
		t.Errorf("in sync: exit %d %q %q", code, stdout, stderr)
	}
	assertNoSecrets(t, app, stdout, stderr)

	// File edited outside depper counts as drift even when keys match.
	edited := hostWith(same, body)
	edited.liveSHA = engine.SHA256Hex([]byte("edited"))
	code, stdout, _, _ = runSecrets(t, secretsRemote(edited, true), "", "secrets", "diff", repo)
	if code != exitcode.Drift || !strings.Contains(stdout, "changed outside depper") {
		t.Errorf("modified file: exit %d %q", code, stdout)
	}
}

func TestSecretsPushSuccess(t *testing.T) {
	repo := secretsRepo(t)
	host := hostWith(map[string]string{"TOKEN": "old", "OLD": "x"}, "old body")
	code, stdout, stderr, app := runSecrets(t, secretsRemote(host, true), "", "secrets", "push", repo)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	for _, want := range []string{"Pushed the environment of api", secretsTestRelease, "1 added (KEEP)", "1 changed (TOKEN)", "1 removed (OLD)"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q: %q", want, stdout)
		}
	}
	assertNoSecrets(t, app, stdout, stderr)
	var restarted bool
	for _, s := range recorderOf(app).Scripts() {
		if strings.Contains(s, "systemctl --user restart depper-api.service") {
			restarted = true
		}
	}
	if !restarted {
		t.Error("service was not restarted")
	}

	code, stdout, stderr, app = runSecrets(t, secretsRemote(host, true), "", "--json", "secrets", "push", repo)
	if code != 0 {
		t.Fatalf("json exit %d: %s", code, stderr)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(stdout), &res); err != nil || res["release"] != secretsTestRelease || res["service"] != "api" {
		t.Errorf("push json = %s (%v)", stdout, err)
	}
	assertNoSecrets(t, app, stdout, stderr)
}

func TestSecretsPushFailureRestores(t *testing.T) {
	repo := secretsRepo(t)
	host := hostWith(map[string]string{"TOKEN": "old"}, "old body")
	code, stdout, stderr, app := runSecrets(t, secretsRemote(host, false), "", "secrets", "push", repo)
	if code != exitcode.Failure || !strings.Contains(stderr, "previous environment was restored") {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
	if strings.Contains(stdout, "Pushed") {
		t.Errorf("reported success: %q", stdout)
	}
	assertNoSecrets(t, app, stdout, stderr)
}

func TestSecretsRejectEnvWithEnvStdin(t *testing.T) {
	repo := secretsRepo(t)
	for _, sub := range []string{"diff", "push"} {
		code, _, stderr, app := runSecrets(t, secretsRemote(hostEnv{}, true), "A=1\n", "secrets", sub, repo, "--env", "x.env", "--env-stdin")
		if code != exitcode.Failure || !strings.Contains(stderr, "cannot be combined") {
			t.Errorf("%s: exit %d %q", sub, code, stderr)
		}
		if len(recorderOf(app).Calls()) != 0 {
			t.Errorf("%s: remote contacted", sub)
		}
	}
}

func TestSecretsMissingEnvFileFailsBeforeRemote(t *testing.T) {
	repo := repoWith(t, secretsToml, nil) // [secrets].file points at a missing file
	for _, sub := range []string{"diff", "push"} {
		code, _, stderr, app := runSecrets(t, secretsRemote(hostEnv{}, true), "", "secrets", sub, repo)
		if code != exitcode.Failure || !strings.Contains(stderr, "not readable") {
			t.Errorf("%s: exit %d %q", sub, code, stderr)
		}
		if len(recorderOf(app).Calls()) != 0 {
			t.Errorf("%s: remote commands ran before the dotenv file was validated", sub)
		}
	}
}
