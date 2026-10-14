package engine

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/frgmt0/depper/internal/shell"
	"github.com/frgmt0/depper/internal/unit"
)

// engineWrap is the prefix of every container-mode Exec*= line: the engine
// runs through /bin/sh -c EngineWrapperScript.
var engineWrap = "/bin/sh -c " + unit.Arg(EngineWrapperScript()) + " "

// foreignXDGEnv is an environment as the systemd user manager may hand it to
// a unit when pam_env or environment.d name another user's directories: a
// runtime directory the test user does not own and base directories it
// cannot write to.
func foreignXDGEnv(extra ...string) []string {
	return append([]string{
		"PATH=" + os.Getenv("PATH"),
		"XDG_RUNTIME_DIR=/",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=//bus",
		"XDG_CONFIG_HOME=/",
		"XDG_DATA_HOME=/nonexistent-depper-test/data",
	}, extra...)
}

func envLines(out string) map[string]string {
	vars := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			vars[k] = v
		}
	}
	return vars
}

func TestStartScriptLayout(t *testing.T) {
	got := StartScript("python3 app.py --label 'it works'")
	want := "#!/bin/sh\n" + strings.Join(shell.SessionEnv(), "\n") + "\nexec python3 app.py --label 'it works'\n"
	if got != want {
		t.Errorf("StartScript = %q, want %q", got, want)
	}
}

// The process-mode start script replaces a foreign XDG_RUNTIME_DIR and drops
// unusable XDG base directories before exec'ing the command, and leaves no
// helper variables in the command's environment.
func TestStartScriptSanitisesInheritedXDG(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root owns and can write every directory used here")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, StartScriptName)
	if err := os.WriteFile(script, []byte(StartScript("env")), 0o700); err != nil {
		t.Fatal(err)
	}
	shells := []string{"/bin/sh"}
	if dash, err := exec.LookPath("dash"); err == nil {
		shells = append(shells, dash)
	}
	run := "/run/user/" + strconv.Itoa(os.Geteuid())
	for _, sh := range shells {
		cmd := exec.Command(sh, script)
		cmd.Env = foreignXDGEnv("XDG_CACHE_HOME=" + dir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v %s", sh, err, out)
		}
		vars := envLines(string(out))
		if vars["XDG_RUNTIME_DIR"] != run || vars["DBUS_SESSION_BUS_ADDRESS"] != "unix:path="+run+"/bus" {
			t.Errorf("%s: runtime dir not replaced:\n%s", sh, out)
		}
		for _, gone := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "depper_xdg", "depper_dir"} {
			if v, ok := vars[gone]; ok {
				t.Errorf("%s: %s=%q reached the command", sh, gone, v)
			}
		}
		if vars["XDG_CACHE_HOME"] != dir {
			t.Errorf("%s: usable XDG_CACHE_HOME dropped:\n%s", sh, out)
		}
	}
}

// systemdArgv splits an Exec*= command line the way systemd does for the
// subset depper renders: double-quoted words with \\ and \" escapes, "$$"
// and "%%" for literal "$" and "%", and unquoted $VAR words replaced by the
// variable's value split at whitespace. Anything else fails the test.
func systemdArgv(t *testing.T, line string, env map[string]string) []string {
	t.Helper()
	var argv []string
	for i := 0; i < len(line); {
		switch c := line[i]; {
		case c == ' ':
			i++
		case c == '"':
			var b strings.Builder
			i++
			for ; i < len(line) && line[i] != '"'; i++ {
				switch {
				case line[i] == '\\' && i+1 < len(line) && (line[i+1] == '\\' || line[i+1] == '"'):
					i++
					b.WriteByte(line[i])
				case line[i] == '\\':
					t.Fatalf("unexpected escape at %d in %s", i, line)
				case (line[i] == '$' || line[i] == '%') && i+1 < len(line) && line[i+1] == line[i]:
					b.WriteByte(line[i])
					i++
				case line[i] == '$' || line[i] == '%':
					t.Fatalf("systemd would expand %q at %d in %s", line[i:], i, line)
				default:
					b.WriteByte(line[i])
				}
			}
			if i >= len(line) {
				t.Fatalf("unterminated quote in %s", line)
			}
			i++
			argv = append(argv, b.String())
		default:
			end := strings.IndexByte(line[i:], ' ')
			if end < 0 {
				end = len(line) - i
			}
			word := line[i : i+end]
			i += end
			if strings.HasPrefix(word, "$") && len(word) > 1 {
				argv = append(argv, strings.Fields(env[word[1:]])...)
				continue
			}
			if strings.ContainsAny(word, `$%\"'`) {
				t.Fatalf("unquoted word %q needs escaping", word)
			}
			argv = append(argv, word)
		}
	}
	return argv
}

func unitDirective(t *testing.T, unitFile, key string) string {
	t.Helper()
	for _, line := range strings.Split(unitFile, "\n") {
		if v, ok := strings.CutPrefix(line, key+"="); ok {
			return v
		}
	}
	t.Fatalf("no %s= in unit:\n%s", key, unitFile)
	return ""
}

// fakeEngine writes an engine stand-in (in a directory whose name has a
// space) that prints its environment and argv.
func fakeEngine(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "engine dir")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "docker")
	script := "#!/bin/sh\nenv\nfor a in \"$@\"; do printf 'ARG<%s>\\n' \"$a\"; done\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func runEngineLine(t *testing.T, line string, env map[string]string) (map[string]string, []string) {
	t.Helper()
	argv := systemdArgv(t, line, env)
	if len(argv) < 3 || argv[0] != "/bin/sh" || argv[1] != "-c" || argv[2] != EngineWrapperScript() {
		t.Fatalf("not wrapped in /bin/sh -c: %q", argv)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = foreignXDGEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	var args []string
	for _, l := range strings.Split(string(out), "\n") {
		if a, ok := strings.CutPrefix(l, "ARG<"); ok {
			args = append(args, strings.TrimSuffix(a, ">"))
		}
	}
	return envLines(string(out)), args
}

func checkSanitised(t *testing.T, name string, vars map[string]string) {
	t.Helper()
	run := "/run/user/" + strconv.Itoa(os.Geteuid())
	if vars["XDG_RUNTIME_DIR"] != run || vars["DBUS_SESSION_BUS_ADDRESS"] != "unix:path="+run+"/bus" {
		t.Errorf("%s: engine got XDG_RUNTIME_DIR=%q DBUS_SESSION_BUS_ADDRESS=%q", name, vars["XDG_RUNTIME_DIR"], vars["DBUS_SESSION_BUS_ADDRESS"])
	}
	for _, gone := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "depper_xdg", "depper_dir"} {
		if v, ok := vars[gone]; ok {
			t.Errorf("%s: %s=%q reached the engine", name, gone, v)
		}
	}
}

// The wrapper script on its own: sanitised environment, and the engine
// receives every argument unchanged (spaces, quotes, $, %, backslashes).
func TestEngineWrapperScriptPassesArgvAndSanitisesEnv(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root owns and can write every directory used here")
	}
	engine := fakeEngine(t)
	args := []string{"run", "a b", `it's "quoted"`, "$HOME", "100%", `back\slash`, ""}
	cmd := exec.Command("/bin/sh", append([]string{"-c", EngineWrapperScript(), engine}, args...)...)
	cmd.Env = foreignXDGEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	checkSanitised(t, "wrapper", envLines(string(out)))
	if want := strings.Join(func() []string {
		w := make([]string, len(args))
		for i, a := range args {
			w[i] = "ARG<" + a + ">"
		}
		return w
	}(), "\n") + "\n"; !strings.Contains(string(out), want) {
		t.Errorf("argv not passed through:\n%s\nwant:\n%s", out, want)
	}
}

// Every engine invocation in rendered build/image and compose units runs
// through the wrapper; executing the lines as systemd would (with
// $DEPPER_CONTAINER_ENV_ARGS split into words) gives the engine a sanitised
// environment and exactly the intended argv.
func TestContainerUnitEngineLinesSanitiseEnv(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root owns and can write every directory used here")
	}
	engine := fakeEngine(t)
	cfg := testConfig(t, "[service]\nname = \"web\"\n[deploy]\nhost = \"server\"\n[container]\nimage = \"nginx:1.25\"\nports = [\"127.0.0.1:8080:80\"]\nargs = [\"--label\", \"x=\\\"a b\\\" $HOME 100%\"]\ncommand = \"serve --label 'it''s here' \\\"q \\\\\\\"x\\\\\\\"\\\"\"\n")
	rendered, err := ContainerUnit(cfg, "/srv/web", engine, "release-20260101T000000Z-00000000")
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{ContainerEnvArgsVar: "--env A --env B"}
	name := cfg.ContainerName()
	cmdWords, err := SplitWords(cfg.Container.Command)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		key  string
		want []string
	}{
		{"ExecStartPre", []string{"rm", "-f", name}},
		{"ExecStart", append([]string{"run", "--rm", "--name", name, "--env", "A", "--env", "B", "-p", "127.0.0.1:8080:80", "--label", `x="a b" $HOME 100%`, cfg.ImageRef("release-20260101T000000Z-00000000")}, cmdWords...)},
		{"ExecStop", []string{"stop", name}},
	} {
		line := strings.TrimPrefix(unitDirective(t, rendered, tt.key), "-")
		vars, args := runEngineLine(t, line, env)
		checkSanitised(t, tt.key, vars)
		if fmt.Sprintf("%q", args) != fmt.Sprintf("%q", tt.want) {
			t.Errorf("%s argv = %q, want %q", tt.key, args, tt.want)
		}
	}
	if fmt.Sprintf("%q", cmdWords) != fmt.Sprintf("%q", []string{"serve", "--label", "its here", `q "x"`}) {
		t.Errorf("command words = %q", cmdWords)
	}

	compose := testConfig(t, "[service]\nname = \"shop\"\n[deploy]\nhost = \"server\"\n[container]\nengine = \"podman\"\ncompose = \"compose.yaml\"\n")
	rendered, err = ContainerUnit(compose, "/srv/shop", engine, "release-20260101T000000Z-00000000")
	if err != nil {
		t.Fatal(err)
	}
	project := ComposeProject(compose)
	for _, tt := range []struct {
		key  string
		want []string
	}{
		{"ExecStart", []string{"compose", "-p", project, "-f", "compose.yaml", "--env-file", "/dev/null", "up", "--build", "--remove-orphans", "--force-recreate"}},
	} {
		vars, args := runEngineLine(t, unitDirective(t, rendered, tt.key), nil)
		checkSanitised(t, "compose "+tt.key, vars)
		if fmt.Sprintf("%q", args) != fmt.Sprintf("%q", tt.want) {
			t.Errorf("compose %s argv = %q, want %q", tt.key, args, tt.want)
		}
	}
}

// The compose cleanup (ExecStartPre=) and stop (ExecStop=) lines run the
// engine with a sanitised environment and the project and file unchanged,
// whatever characters they hold.
func TestComposeCleanupLinesSanitiseEnv(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root owns and can write every directory used here")
	}
	engine := fakeEngine(t)
	cfg := testConfig(t, "[service]\nname = \"shop\"\n[deploy]\nhost = \"server\"\n[container]\nengine = \"docker\"\ncompose = \"deploy/compose.prod.yaml\"\n")
	rendered, err := ContainerUnit(cfg, "/srv/shop", engine, "release-20260101T000000Z-00000000")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rendered, "podman.socket") {
		t.Error("docker compose unit depends on podman.socket")
	}
	project := ComposeProject(cfg)
	for key, script := range map[string]string{"ExecStartPre": ComposeCleanupScript(), "ExecStop": ComposeStopScript()} {
		line := unitDirective(t, rendered, key)
		if key == "ExecStartPre" && !strings.HasPrefix(line, "-") {
			t.Errorf("ExecStartPre failure is not ignored: %s", line)
		}
		argv := systemdArgv(t, strings.TrimPrefix(line, "-"), nil)
		if fmt.Sprintf("%q", argv) != fmt.Sprintf("%q", []string{"/bin/sh", "-c", script, engine, project, "deploy/compose.prod.yaml"}) {
			t.Fatalf("%s argv = %q", key, argv)
		}
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Env = foreignXDGEnv()
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v %s", key, err, out)
		}
		checkSanitised(t, key, envLines(string(out)))
		if !strings.Contains(string(out), "ARG<compose>\nARG<-p>\nARG<"+project+">\nARG<-f>\nARG<deploy/compose.prod.yaml>\nARG<--env-file>\nARG</dev/null>\nARG<down>\n") {
			t.Errorf("%s did not run compose down with the project and file:\n%s", key, out)
		}
	}
}
