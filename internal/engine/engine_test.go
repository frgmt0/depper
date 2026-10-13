package engine

import (
	"context"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/frgmt0/depper/internal/config"
	"github.com/frgmt0/depper/internal/transport"
	"github.com/frgmt0/depper/internal/transport/transporttest"
)

// scriptedExec answers every ssh script with fn(script).
type scriptedExec func(script string) (string, error)

func (f scriptedExec) Run(_ context.Context, c transport.Cmd) (transport.Result, error) {
	data := ""
	if c.Stdin != nil {
		raw, _ := io.ReadAll(c.Stdin)
		data = string(raw)
	}
	out, err := f(data)
	if !c.Capture && c.Stdout != nil {
		_, _ = io.WriteString(c.Stdout, out)
		out = ""
	}
	return transport.Result{Stdout: out}, err
}

func testConfig(t *testing.T, toml string) *config.Config {
	t.Helper()
	return loadConfig(t, writeRepo(t, toml, nil))
}

func recorderEngine(cfg *config.Config, handler func(transporttest.Call) (transport.Result, error)) (*Engine, *transporttest.Recorder) {
	rec := &transporttest.Recorder{Handler: handler}
	e := New(cfg, transport.NewRemote(rec, cfg.SSHTarget()), rec, io.Discard)
	return e, rec
}

func TestProbeRejectsInvalidPathAndLegacyLayout(t *testing.T) {
	cfg := testConfig(t, "[service]\nname = \"api\"\ncommand = \"x\"\n[deploy]\nhost = \"server\"\n")
	for _, tc := range []struct{ out, want string }{
		{"base=relative/path\nhome=/h\nlayout=none\ntunnel=no\n", "invalid deployment path"},
		{"base=/srv/a b\nhome=/h\nlayout=none\ntunnel=no\n", "invalid deployment path"},
		{"base=/srv/api\nbase=/x\nlayout=none\n", "invalid deployment probe"},
		{"base=/srv/api\nhome=/h\nlayout=weird\ntunnel=no\n", "invalid deployment probe"},
		{"base=/srv/api\nhome=/h\nlayout=legacy\ntunnel=no\n", "depper migrate"},
	} {
		e, _ := recorderEngine(cfg, func(transporttest.Call) (transport.Result, error) {
			return transport.Result{Stdout: tc.out}, nil
		})
		if _, err := e.Probe(context.Background(), true); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("probe %q: err = %v, want %q", tc.out, err, tc.want)
		}
	}
	e, rec := recorderEngine(cfg, func(transporttest.Call) (transport.Result, error) {
		return transport.Result{Stdout: "base=/home/u/.local/share/depper/api\nhome=/home/u\nlayout=depper\ntunnel=yes\n"}, nil
	})
	l, err := e.Probe(context.Background(), true)
	if err != nil || l.Base != "/home/u/.local/share/depper/api" || !l.TunnelOwned || !l.Deployed() {
		t.Fatalf("layout = %+v (%v)", l, err)
	}
	script := rec.Scripts()[0]
	for _, want := range []string{`base="$HOME/.local/share/depper/api"`, `legacy="$HOME/.local/share/deployer/api"`, "deployer-api.service", "chmod 700", "Refusing a symlink deployment metadata directory"} {
		if !strings.Contains(script, want) {
			t.Errorf("probe script missing %q", want)
		}
	}
	checkScriptSyntax(t, []string{script, e.probeScript(false)})
}

func TestUploadScriptIsPrivateAtomicAndNeverContainsTheValue(t *testing.T) {
	script, err := uploadScript(UploadFile{[]byte("TOKEN=" + sentinel + "\n"), "/srv/api/.depper/environment.next-r1", "600"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"umask 077", "test ! -L", "mktemp", "trap", `mv -f "$temporary" "$destination"`, "base64 -d", "chmod 600"} {
		if !strings.Contains(script, want) {
			t.Errorf("upload script missing %q", want)
		}
	}
	if strings.Contains(script, sentinel) {
		t.Error("upload script contains the raw value")
	}
	if _, err := uploadScript(UploadFile{nil, "/srv/a b", "600"}); err == nil {
		t.Error("non-plain destination accepted")
	}
	if _, err := uploadScript(UploadFile{nil, "/srv/x", "777"}); err == nil {
		t.Error("mode 777 accepted")
	}
	checkScriptSyntax(t, []string{script})
}

func TestSetupAndHealthScriptsExportOnlyEnvAndNeverSourceSecrets(t *testing.T) {
	cfg := testConfig(t, "[service]\nname = \"api\"\ncommand = \"x\"\n[deploy]\nhost = \"server\"\nsetup = [\"python3 setup.py\"]\n[env]\nPAYLOAD = \"$(printf should-not-run)\"\nSAFE = \"yes\"\n[health]\ncommand = \"python3 health.py\"\n")
	e, rec := recorderEngine(cfg, nil)
	l := &Layout{Base: "/srv/api", State: LayoutDepper}
	if err := e.runSetup(context.Background(), l, "/srv/api/releases/r1", "r1"); err != nil {
		t.Fatal(err)
	}
	setup := rec.Scripts()[0]
	health := e.commandScript(l)
	for _, script := range []string{setup, health} {
		if strings.Contains(script, ". ") && strings.Contains(script, "environment") {
			t.Errorf("script sources the environment file:\n%s", script)
		}
		if !strings.Contains(script, "export PAYLOAD='$(printf should-not-run)'") || !strings.Contains(script, "export SAFE=yes") {
			t.Errorf("script does not export [env] safely:\n%s", script)
		}
	}
	if !strings.Contains(setup, "cd /srv/api/releases/r1") || !strings.Contains(setup, "/bin/sh -lc 'python3 setup.py'") {
		t.Errorf("setup = %s", setup)
	}
	if !strings.Contains(health, "cd /srv/api/current") || !strings.Contains(health, "/bin/sh -lc 'python3 health.py'") {
		t.Errorf("health = %s", health)
	}
	checkScriptSyntax(t, []string{setup, health})
}

func TestRestoreScriptRemovesCurrentAndStopsServiceOnFirstDeploy(t *testing.T) {
	cfg := testConfig(t, "[service]\nname = \"api\"\ncommand = \"x\"\n[deploy]\nhost = \"server\"\n")
	e, _ := recorderEngine(cfg, nil)
	l := &Layout{Base: "/srv/api", State: LayoutDepper}
	script := e.restoreScript(l, "release-20260101T000000Z-00000000")
	for _, want := range []string{
		`elif test -f "$tx/no-$2"; then rm -f "$1"; fi; }`,
		"restore_link \"$base/current\" current",
		"systemctl --user disable --now depper-api.service || true",
		`if test -f "$tx/unit"; then systemctl --user restart depper-api.service; fi`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("restore script missing %q", want)
		}
	}
	activate := e.activateScript(l, "release-20260101T000000Z-00000000")
	if !strings.Contains(activate, "ln -s releases/release-20260101T000000Z-00000000") || !strings.Contains(activate, `mv -Tf "$base/.current.next" "$base/current"`) {
		t.Errorf("activate does not switch a relative current link:\n%s", activate)
	}
	checkScriptSyntax(t, []string{script, activate, e.commitScript(l, "release-20260101T000000Z-00000000"), e.listScript(l), e.statusScript(l), e.inspectScript(), curlScript(&config.Health{URL: "http://127.0.0.1:1/x", Timeout: time.Second})})
}

func TestProcessUnitEscaping(t *testing.T) {
	got := ProcessUnit("api", "always", "/srv/a$b")
	for _, want := range []string{
		"Description=depper: api",
		"WorkingDirectory=/srv/a$b/current",
		"EnvironmentFile=/srv/a$b/.depper/environment",
		`ExecStart=/bin/sh "/srv/a$$b/current/.depper-start.sh"`,
		"Restart=always",
		"WantedBy=default.target",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("unit missing %q:\n%s", want, got)
		}
	}
}

func TestContainerEnvFile(t *testing.T) {
	got, err := ContainerEnvFile(map[string]string{"B": `say "hi" $X`, "A": "1"})
	if err != nil || got != "A=1\nB=say \"hi\" $X\n" {
		t.Errorf("got %q, %v", got, err)
	}
	for _, bad := range []string{"a\nb", "a\rb", "a\x00b"} {
		_, err := ContainerEnvFile(map[string]string{"SECRET": bad})
		if err == nil || !strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), bad) {
			t.Errorf("value %q: err = %v", bad, err)
		}
	}
}

func TestSplitWords(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"serve", []string{"serve"}},
		{"  a  b ", []string{"a", "b"}},
		{`a 'b c' "d \"e\" \$f" g\ h`, []string{"a", "b c", `d "e" $f`, "g h"}},
		{`x '' y`, []string{"x", "", "y"}},
		{`a"b"'c'`, []string{"abc"}},
	}
	for _, tc := range cases {
		got, err := SplitWords(tc.in)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("SplitWords(%q) = %q, %v", tc.in, got, err)
		}
	}
	for _, bad := range []string{"'open", `"open`, `trailing\`, "   "} {
		if _, err := SplitWords(bad); err == nil {
			t.Errorf("SplitWords(%q) accepted", bad)
		}
	}
}

func TestFingerprintsAndComparison(t *testing.T) {
	salt := []byte(strings.Repeat("s", SaltBytes))
	a := Fingerprints(salt, map[string]string{"A": "1", "B": "2", "C": "3"})
	b := Fingerprints(salt, map[string]string{"A": "1", "B": "changed", "D": "4"})
	if a["A"] != b["A"] || a["B"] == b["B"] || len(a["A"]) != 64 {
		t.Fatal("fingerprints are not deterministic HMACs")
	}
	if other := Fingerprints([]byte(strings.Repeat("t", SaltBytes)), map[string]string{"A": "1"}); other["A"] == a["A"] {
		t.Error("salt does not affect fingerprints")
	}
	k := CompareFingerprints(a, b)
	if !k.Known || !reflect.DeepEqual(k.Added, []string{"D"}) || !reflect.DeepEqual(k.Removed, []string{"C"}) || !reflect.DeepEqual(k.Changed, []string{"B"}) || !reflect.DeepEqual(k.Unchanged, []string{"A"}) || k.InSync() {
		t.Errorf("changes = %+v", k)
	}
	if s := k.Summary(); s != "1 added (D), 1 changed (B), 1 removed (C), 1 unchanged" {
		t.Errorf("summary = %q", s)
	}
	if u := CompareFingerprints(nil, a); u.Known || len(u.Added) != 3 || !strings.Contains(u.Summary(), "no previous manifest") {
		t.Errorf("unknown = %+v", u)
	}
	m := NewManifest(salt, []byte("A=\"1\"\n"), map[string]string{"A": "1"}, time.Unix(0, 0))
	parsed, err := ParseManifest(string(m.Encode()))
	if err != nil || !reflect.DeepEqual(parsed, m) || parsed.UpdatedAt != "1970-01-01T00:00:00Z" {
		t.Errorf("round trip = %+v (%v)", parsed, err)
	}
	for _, bad := range []string{"{", `{"file_sha256":"x","keys":{}}`, `{"file_sha256":"` + strings.Repeat("a", 64) + `","keys":{"bad key":"` + strings.Repeat("a", 64) + `"}}`} {
		if _, err := ParseManifest(bad); err == nil {
			t.Errorf("ParseManifest(%q) accepted", bad)
		}
	}
}

func TestParseSecretsState(t *testing.T) {
	salt := strings.Repeat("ab", SaltBytes)
	sum := strings.Repeat("c", 64)
	manifest := `{"file_sha256":"` + sum + `","keys":{},"updated_at":"t"}`
	st, err := parseSecretsState("@@salt\n" + salt + "\n@@manifest\n" + manifest + "\n@@live\n" + sum + "  -\n")
	if err != nil || len(st.Salt) != SaltBytes || st.Manifest == nil || !st.LiveExists || st.FileModified() {
		t.Fatalf("state = %+v (%v)", st, err)
	}
	st, _ = parseSecretsState("@@salt\n@@manifest\n" + manifest + "\n@@live\n")
	if st.Salt != nil || !st.FileModified() {
		t.Errorf("missing live file not reported: %+v", st)
	}
	if _, err := parseSecretsState("@@salt\nnothex\n@@manifest\n@@live\n"); err == nil {
		t.Error("invalid salt accepted")
	}
}

func TestParseSectionsAndReleaseList(t *testing.T) {
	out := "@@current\nreleases/release-20260102T000000Z-00000002\n@@previous\nreleases/release-20260101T000000Z-00000001\n" +
		"@@previous-transaction\ntransactions/release-20260102T000000Z-00000002\n@@previous-transaction-release\nreleases/release-20260101T000000Z-00000001\n" +
		"@@release release-20260101T000000Z-00000001\nnone\n" +
		"@@release release-20260102T000000Z-00000002\napplied\n{\"id\":\"release-20260102T000000Z-00000002\",\"git_commit\":\"abc\"}\n" +
		"@@release not-a-release\nnone\n@@tx release-20260102T000000Z-00000002\n@@tx secrets-x\n"
	list := parseReleaseList(out)
	if len(list.Releases) != 2 || list.Releases[0].ID != "release-20260102T000000Z-00000002" || !list.Releases[0].Current || list.Releases[0].Metadata.GitCommit != "abc" {
		t.Fatalf("list = %+v", list)
	}
	old := list.Releases[1]
	if !old.Previous || !old.RollbackAvailable || old.applied {
		t.Errorf("legacy rollback source not detected: %+v", old)
	}
	if len(list.transactions) != 2 {
		t.Errorf("transactions = %v", list.transactions)
	}
	del, kept := pruneSet(&ReleaseList{Current: "c", Previous: "a", Releases: []ReleaseInfo{{ID: "e"}, {ID: "d"}, {ID: "c"}, {ID: "b"}, {ID: "a"}}}, 2)
	if !reflect.DeepEqual(del, []string{"b"}) || len(kept) != 4 {
		t.Errorf("prune = %v %v", del, kept)
	}
	secs := parseSections("ignored\n@@a\nx\n@@body\n@@not-a-section\nraw", "body")
	if len(secs) != 2 || secs[1].Body != "@@not-a-section\nraw" {
		t.Errorf("sections = %+v", secs)
	}
}

func TestReleaseFromLink(t *testing.T) {
	id := "release-20260101T000000Z-0123abcd"
	for in, want := range map[string]string{
		"releases/" + id + "\n":                            id,
		"/home/u/.local/share/deployer/api/releases/" + id: id,
		"releases/other":                                   "",
		"elsewhere/" + id:                                  "",
		"":                                                 "",
	} {
		if got := releaseFromLink(in); got != want {
			t.Errorf("releaseFromLink(%q) = %q", in, got)
		}
	}
}

func TestHealthBodyEncoding(t *testing.T) {
	if got := string(encodeBody([]byte(" {\"a\":1}\n"))); got != `{"a":1}` {
		t.Errorf("json body = %s", got)
	}
	if got := string(encodeBody(nil)); got != "null" {
		t.Errorf("empty body = %s", got)
	}
	long := strings.Repeat("x", BodyLimit+100)
	if got := string(encodeBody([]byte(long))); len(got) != BodyLimit+2 {
		t.Errorf("body not truncated: %d", len(got))
	}
}
