package shell

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestQuoteMatchesShlex(t *testing.T) {
	cases := map[string]string{
		"":                    "''",
		"plain":               "plain",
		"/srv/app-1.2/x_y":    "/srv/app-1.2/x_y",
		"a@b%c+d=e:f,g":       "a@b%c+d=e:f,g",
		"two words":           "'two words'",
		"it's":                `'it'"'"'s'`,
		"$HOME":               "'$HOME'",
		"`id`":                "'`id`'",
		"line\nbreak":         "'line\nbreak'",
		"semi;colon":          "'semi;colon'",
		"ünïcode":             "'ünïcode'",
		"glob*":               "'glob*'",
		"tilde~":              "'tilde~'",
		"back\\slash":         "'back\\slash'",
		"double\"quote":       "'double\"quote'",
		"'":                   `''"'"''`,
		"-rf":                 "-rf",
		"x=1 && echo pwned &": "'x=1 && echo pwned &'",
	}
	for in, want := range cases {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestQuoteRoundTripsThroughSh(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	for _, value := range []string{"", "a b", "it's", "$(id)", "`id`", "x\ny", "\\n", "\"q\"", "*", "~", "a'b'c"} {
		out, err := exec.Command(sh, "-c", "printf '%s' "+Quote(value)).Output()
		if err != nil {
			t.Fatalf("sh failed for %q: %v", value, err)
		}
		if string(out) != value {
			t.Errorf("round trip of %q gave %q", value, out)
		}
	}
}

func TestScriptAndJoin(t *testing.T) {
	if got := Script("set -eu", "echo hi"); got != "set -eu\necho hi\n" {
		t.Errorf("Script = %q", got)
	}
	if got := Join("systemctl", "--user", "restart", "depper-a b.service"); got != "systemctl --user restart 'depper-a b.service'" {
		t.Errorf("Join = %q", got)
	}
}

func TestExportsAreSortedAndQuoted(t *testing.T) {
	got := Exports(map[string]string{"B": "two words", "A": "$x"})
	want := []string{"export A='$x'", "export B='two words'"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("Exports = %q, want %q", got, want)
	}
}

func TestPreludeIsPOSIX(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	script := Script(append(Prelude(), `printf '%s' "$XDG_RUNTIME_DIR"`)...)
	cmd := exec.Command(sh, "-n")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("prelude is not valid sh: %v %s", err, out)
	}
}

func TestIsPlainPath(t *testing.T) {
	good := []string{"/", "/srv/api", "/home/u/.local/share/depper/api", "/a-b_c.d/e"}
	bad := []string{"", "srv/api", "~/x", "/srv/a b", "/srv/a\tb", "/srv/a b", "/srv/a%b", "/srv/a'b", "/srv/a\"b", "/srv/a\\b", "/srv/a\nb", "/srv/a\rb", "/srv/a\x00b"}
	for _, p := range good {
		if !IsPlainPath(p) {
			t.Errorf("IsPlainPath(%q) = false", p)
		}
	}
	for _, p := range bad {
		if IsPlainPath(p) {
			t.Errorf("IsPlainPath(%q) = true", p)
		}
	}
}

func TestControlHelpers(t *testing.T) {
	if !HasLineBreak("a\rb") || !HasLineBreak("a\x00") || HasLineBreak("a\tb") {
		t.Error("HasLineBreak misclassifies")
	}
	if !HasControl("a\tb") || !HasControl("a\x7f") || HasControl("plain ü") {
		t.Error("HasControl misclassifies")
	}
}

func TestIsSafePath(t *testing.T) {
	good := []string{"/srv/api", "/home/u/.local/share/depper/api", "/a-b_c.d/e", "/srv/a+b@c", "/srv/api/releases/release-20261012T100000Z-0123abcd/"}
	bad := []string{"", "/", "//", "srv/api", "~/x", "/srv/a b", "/srv/a'b", "/srv/a\"b", "/srv/a%b", "/srv/a$b", "/srv/a:b", "/srv/a*b", "/srv/a\nb", "/srv/ü", "/srv/a\\b", "/srv/a,b"}
	for _, p := range good {
		if !IsSafePath(p) {
			t.Errorf("IsSafePath(%q) = false", p)
		}
	}
	for _, p := range bad {
		if IsSafePath(p) {
			t.Errorf("IsSafePath(%q) = true", p)
		}
	}
}

// A wrapped script that arrives truncated must run nothing at all.
func TestWrapRunsNothingWhenTruncated(t *testing.T) {
	sh := "sh"
	if p, err := exec.LookPath("dash"); err == nil {
		sh = p
	}
	dir := t.TempDir()
	marker := dir + "/ran"
	script := Wrap(Script("set -eu", "touch "+Quote(marker), "echo second"))
	if !strings.HasPrefix(script, "set +xv\n{\n") || !strings.HasSuffix(script, "\n}\n") {
		t.Fatalf("Wrap = %q", script)
	}
	truncated := script[:strings.Index(script, "echo second")]
	cmd := exec.Command(sh, "-s")
	cmd.Stdin = strings.NewReader(truncated)
	if err := cmd.Run(); err == nil {
		t.Error("truncated script succeeded")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("truncated script ran its first command")
	}
	cmd = exec.Command(sh, "-s")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err != nil || string(out) != "second\n" {
		t.Fatalf("full script: %v %q", err, out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("full script did not run")
	}
	if Wrap("echo x") != "set +xv\n{\necho x\n}\n" {
		t.Errorf("Wrap without trailing newline = %q", Wrap("echo x"))
	}
}

// An inherited XDG_RUNTIME_DIR that is missing or owned by another user
// (pam_env applying /etc/environment, as on the GitHub runner image) is
// replaced, with the bus address; the session's own one is kept.
func TestSessionEnvReplacesForeignRuntimeDir(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	if os.Geteuid() == 0 {
		t.Skip("root owns every directory used here")
	}
	own := t.TempDir()
	run := "/run/user/" + strconv.Itoa(os.Geteuid())
	for _, tt := range []struct {
		name, xdg, dbus, wantXDG, wantDBus string
	}{
		{"unset", "", "", run, "unix:path=" + run + "/bus"},
		{"another user's", "/", "unix:path=//bus", run, "unix:path=" + run + "/bus"},
		{"missing", own + "/gone", "", run, "unix:path=" + run + "/bus"},
		{"own", own, "unix:path=" + own + "/custom", own, "unix:path=" + own + "/custom"},
		{"own without bus", own, "", own, "unix:path=" + own + "/bus"},
	} {
		cmd := exec.Command(sh, "-s")
		cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
		if tt.xdg != "" {
			cmd.Env = append(cmd.Env, "XDG_RUNTIME_DIR="+tt.xdg)
		}
		if tt.dbus != "" {
			cmd.Env = append(cmd.Env, "DBUS_SESSION_BUS_ADDRESS="+tt.dbus)
		}
		cmd.Stdin = strings.NewReader(Script(append(Prelude(), `printf '%s\n%s\n' "$XDG_RUNTIME_DIR" "$DBUS_SESSION_BUS_ADDRESS"`, `sh -c 'printf "%s\n" "$XDG_RUNTIME_DIR"'`)...))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v %s", tt.name, err, out)
		}
		want := tt.wantXDG + "\n" + tt.wantDBus + "\n" + tt.wantXDG + "\n"
		if string(out) != want {
			t.Errorf("%s: got %q, want %q", tt.name, out, want)
		}
	}
}

// posixShells returns /bin/sh and, when installed, dash: SessionEnv must run
// under both (dash is /bin/sh on Debian and Ubuntu hosts).
func posixShells(t *testing.T) []string {
	t.Helper()
	var shells []string
	if _, err := os.Stat("/bin/sh"); err == nil {
		shells = append(shells, "/bin/sh")
	}
	if dash, err := exec.LookPath("dash"); err == nil {
		shells = append(shells, dash)
	}
	if len(shells) == 0 {
		t.Skip("no POSIX shell available")
	}
	return shells
}

// writableForeignDir returns a directory the test user can write to but does
// not own (the sticky, world-writable /tmp), or skips.
func writableForeignDir(t *testing.T) string {
	t.Helper()
	for _, dir := range []string{"/tmp", "/var/tmp", "/dev/shm"} {
		real, err := filepath.EvalSymlinks(dir)
		if err != nil {
			continue
		}
		info, err := os.Stat(real)
		if err != nil || !info.IsDir() {
			continue
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(st.Uid) == os.Geteuid() {
			continue
		}
		if syscall.Access(real, 2) != nil { // W_OK
			continue
		}
		return real
	}
	t.Skip("no writable directory owned by another user")
	return ""
}

// XDG base directories only need to be usable: a writable directory is kept
// even when another user owns it, as is a missing one whose parent is
// writable. Unwritable directories, missing ones in an unwritable or missing
// parent, and paths that are not directories are dropped, so the defaults
// under $HOME apply. No helper variables are left behind.
func TestSessionEnvDropsForeignBaseDirs(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write to every directory used here")
	}
	own := t.TempDir()
	file := filepath.Join(own, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	foreign := writableForeignDir(t)
	missingInForeign := filepath.Join(foreign, "depper-test-missing-"+strconv.Itoa(os.Getpid()))
	for _, sh := range posixShells(t) {
		for _, tt := range []struct {
			name, value string
			keep        bool
		}{
			{"unwritable", "/", false},
			{"own", own, true},
			{"missing in own", own + "/new", true},
			{"missing in unwritable", "/depper-test-missing", false},
			{"missing parent", "/nonexistent-depper-test/state", false},
			{"writable, not owned", foreign, true},
			{"missing in writable, not owned", missingInForeign, true},
			{"regular file", file, false},
		} {
			for _, strict := range []string{"set -eu", "set -e"} {
				lines := append([]string{strict}, SessionEnv()...)
				lines = append(lines, `printf '%s|%s|%s|%s|%s|%s\n' "${XDG_CONFIG_HOME-unset}" "${XDG_DATA_HOME-unset}" "${XDG_CACHE_HOME-unset}" "${XDG_STATE_HOME-unset}" "${depper_xdg-gone}" "${depper_dir-gone}"`)
				cmd := exec.Command(sh, "-s")
				cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
				for _, key := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME"} {
					cmd.Env = append(cmd.Env, key+"="+tt.value)
				}
				cmd.Stdin = strings.NewReader(Script(lines...))
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("%s %s (%s): %v %s", sh, tt.name, strict, err, out)
				}
				want := "unset"
				if tt.keep {
					want = tt.value
				}
				if w := strings.Repeat(want+"|", 4) + "gone|gone\n"; string(out) != w {
					t.Errorf("%s %s (%s): got %q, want %q", sh, tt.name, strict, out, w)
				}
			}
		}
	}
}
