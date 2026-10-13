package shell

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
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
