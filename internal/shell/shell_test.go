package shell

import (
	"os/exec"
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
