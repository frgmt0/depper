package unit

import "testing"

func TestArg(t *testing.T) {
	cases := map[string]string{
		"/srv/api/current/.depper-start.sh": `"/srv/api/current/.depper-start.sh"`,
		`a "quoted" \ value`:                `"a \"quoted\" \\ value"`,
		"$HOME and %h":                      `"$$HOME and %%h"`,
	}
	for in, want := range cases {
		if got := Arg(in); got != want {
			t.Errorf("Arg(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPathAndExecPath(t *testing.T) {
	if got := Path(`/srv/my app/100%\x`); got != `/srv/my\x20app/100%%\x5cx` {
		t.Errorf("Path = %q", got)
	}
	if got := Path("/srv/$x"); got != "/srv/$x" {
		t.Errorf("Path must not double $: %q", got)
	}
	if got := ExecPath("/srv/$x y"); got != `/srv/$$x\x20y` {
		t.Errorf("ExecPath = %q", got)
	}
}

func TestEnvironmentFile(t *testing.T) {
	got := EnvironmentFile(map[string]string{"B": "line\nnext", "A": `q"\`})
	want := "A=\"q\\\"\\\\\"\nB=\"line\nnext\"\n"
	if got != want {
		t.Errorf("EnvironmentFile = %q, want %q", got, want)
	}
	if EnvironmentFile(nil) != "" {
		t.Error("empty environment must render empty")
	}
}
