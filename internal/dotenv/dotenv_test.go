package dotenv

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func mustParse(t *testing.T, text string) map[string]string {
	t.Helper()
	values, err := Parse(text)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return values
}

func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func chdir(t *testing.T, dir string) {
	t.Helper()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
}

func TestParsesCommentsExportsQuotesCRLFAndMultilineWithoutExpansion(t *testing.T) {
	values := mustParse(t, "# heading\r\n"+
		"export PORT = 8080 # deployment port\r\n"+
		"HASH=value#is-literal\r\n"+
		"SINGLE='literal $() `backtick` ${VARIABLE} # hash'\r\n"+
		`DOUBLE="line\nnext\t\\slash\"quote"`+"\r\n"+
		"MULTILINE=\"first\nsecond\"\r\n"+
		"UNQUOTED=$() `backtick` ${VARIABLE}\r\n")
	want := map[string]string{
		"PORT":      "8080",
		"HASH":      "value#is-literal",
		"SINGLE":    "literal $() `backtick` ${VARIABLE} # hash",
		"DOUBLE":    "line\nnext\t\\slash\"quote",
		"MULTILINE": "first\nsecond",
		"UNQUOTED":  "$() `backtick` ${VARIABLE}",
	}
	if !reflect.DeepEqual(values, want) {
		t.Errorf("got %#v\nwant %#v", values, want)
	}
}

func TestMalformedDotenvErrorsAreContentSafe(t *testing.T) {
	secret := "synthetic-should-never-appear"
	for _, text := range []string{"TOKEN=" + secret + "\nnot an assignment\n", `TOKEN="` + secret} {
		_, err := Parse(text)
		var dotenvErr *Error
		if !errors.As(err, &dotenvErr) {
			t.Fatalf("expected *Error, got %v", err)
		}
		if !regexp.MustCompile(`line [12]`).MatchString(err.Error()) {
			t.Errorf("error %q does not name a line", err)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error leaks content: %q", err)
		}
	}
}

func TestTrailingCommentsAtEOFPreserveAssignmentAndEmptyUnquotedValue(t *testing.T) {
	values := mustParse(t, "QUOTED=\"value\" # final comment\nEMPTY= # final comment")
	want := map[string]string{"QUOTED": "value", "EMPTY": ""}
	if !reflect.DeepEqual(values, want) {
		t.Errorf("got %#v", values)
	}
}

func TestParseEdgeCases(t *testing.T) {
	cases := []struct {
		name string
		text string
		want map[string]string
	}{
		{"empty", "", map[string]string{}},
		{"only comment no newline", "# x", map[string]string{}},
		{"tab comment", "A=b\t# c", map[string]string{"A": "b"}},
		{"earliest comment wins", "A=b\t#c #d", map[string]string{"A": "b"}},
		{"later key wins", "A=1\nA=2\n", map[string]string{"A": "2"}},
		{"export as key", "export=1", map[string]string{"export": "1"}},
		{"exported key named export", "export export=1", map[string]string{"export": "1"}},
		{"unknown escape kept", `A="\q\$"`, map[string]string{"A": `\q\$`}},
		{"unknown escape multibyte", `A="\é"`, map[string]string{"A": `\é`}},
		{"single quote no escapes", `A='a\nb'`, map[string]string{"A": `a\nb`}},
		{"trailing spaces trimmed", "A=b  \r\n", map[string]string{"A": "b"}},
		{"empty value at eof", "A=", map[string]string{"A": ""}},
		{"unicode value", "A=héllo wörld", map[string]string{"A": "héllo wörld"}},
		{"quoted then crlf", "A='x' \r\nB=y", map[string]string{"A": "x", "B": "y"}},
		{"leading whitespace", "  \t A=1", map[string]string{"A": "1"}},
	}
	for _, tc := range cases {
		got, err := Parse(tc.text)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %#v want %#v", tc.name, got, tc.want)
		}
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"1A=x", "line 1"},
		{"A x", "line 1"},
		{"A", "line 1"},
		{"\n\nA-B=1", "line 3"},
		{"A=\"x\" trailing", "line 1"},
		{"A=\"x\\", "line 1"},
		{"A='x\ny\nB=1", "Unterminated quoted dotenv value at line 1"},
		{"A=\"one\ntwo\" junk", "line 1"},
		{"A=\"one\ntwo\"\n?", "line 3"},
		{"export A", "line 1"},
	}
	for _, tc := range cases {
		_, err := Parse(tc.text)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Parse(%q) error = %v, want containing %q", tc.text, err, tc.want)
		}
	}
}

func TestParseReaderLimitsEncodingBOMAndNUL(t *testing.T) {
	values, err := ParseReader(strings.NewReader("\xef\xbb\xbfA=1\n"))
	if err != nil || values["A"] != "1" {
		t.Fatalf("BOM handling: %v %v", values, err)
	}
	if _, err := ParseReader(strings.NewReader("A=\xff\n")); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Errorf("invalid UTF-8 error = %v", err)
	}
	if _, err := ParseReader(strings.NewReader("A=1\nB=x\x00y\n")); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Errorf("NUL error = %v", err)
	}
	big := "A=" + strings.Repeat("x", Limit)
	if _, err := ParseReader(strings.NewReader(big)); err == nil || !strings.Contains(err.Error(), "1 MiB") {
		t.Errorf("limit error = %v", err)
	}
	exact := "A=" + strings.Repeat("x", Limit-2)
	if values, err := ParseReader(strings.NewReader(exact)); err != nil || len(values["A"]) != Limit-2 {
		t.Errorf("exact-limit input rejected: %v", err)
	}
}

func TestSelectedSourceIsRegularFileAndItsExactRsyncPathsAreExcluded(t *testing.T) {
	repo := realTempDir(t)
	nested := filepath.Join(repo, "ops", "prod[1]*?.env")
	write(t, nested, "TOKEN=synthetic\n")
	alias := filepath.Join(repo, "selected.env")
	if err := os.Symlink(filepath.Join("ops", "prod[1]*?.env"), alias); err != nil {
		t.Fatal(err)
	}
	source, err := ResolveSource(repo, alias)
	if err != nil {
		t.Fatal(err)
	}
	if source.Selected != alias {
		t.Errorf("Selected = %q, want %q", source.Selected, alias)
	}
	if source.Resolved != nested {
		t.Errorf("Resolved = %q, want %q", source.Resolved, nested)
	}
	want := []string{"/selected.env", "/ops/prod[[]1[]][*][?].env"}
	if !reflect.DeepEqual(source.RsyncExcludes, want) {
		t.Errorf("RsyncExcludes = %q, want %q", source.RsyncExcludes, want)
	}
}

func TestMissingOrNonregularSourcesFailBeforeReading(t *testing.T) {
	repo := t.TempDir()
	for _, selected := range []string{filepath.Join(repo, "missing.env"), repo} {
		_, err := ResolveSource(repo, selected)
		if err == nil || !regexp.MustCompile(`not readable|regular file`).MatchString(err.Error()) {
			t.Errorf("ResolveSource(%q) error = %v", selected, err)
		}
	}
}

func TestOversizedSourceIsRejectedBeforeReading(t *testing.T) {
	repo := t.TempDir()
	path := filepath.Join(repo, "big.env")
	write(t, path, "A="+strings.Repeat("x", Limit))
	if _, err := ResolveSource(repo, path); err == nil || !strings.Contains(err.Error(), "1 MiB") {
		t.Errorf("error = %v", err)
	}
}

func TestUnreadableSourceErrorDoesNotEchoContents(t *testing.T) {
	repo := t.TempDir()
	selected := filepath.Join(repo, "private.env")
	write(t, selected, "TOKEN=synthetic")
	source, err := ResolveSource(repo, selected)
	if err != nil {
		t.Fatal(err)
	}
	previous := openFile
	openFile = func(string) (io.ReadCloser, error) { return nil, errors.New("permission denied: synthetic") }
	t.Cleanup(func() { openFile = previous })
	_, err = Read(source)
	if err == nil || !strings.Contains(err.Error(), "not readable") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), "synthetic") {
		t.Errorf("error leaks content: %q", err)
	}
}

func TestEscapedExcludeDoesNotTransferASpecialCharacterSecretFilename(t *testing.T) {
	rsync, err := exec.LookPath("rsync")
	if err != nil {
		t.Skip("rsync not installed")
	}
	root := realTempDir(t)
	repo := filepath.Join(root, "repo")
	destination := filepath.Join(root, "destination")
	write(t, filepath.Join(repo, "public.txt"), "public")
	secret := filepath.Join(repo, "credentials[prod]*?.txt")
	write(t, secret, "synthetic-secret")
	args := []string{"-a"}
	for _, pattern := range RsyncExcludes(repo, secret) {
		args = append(args, "--exclude", pattern)
	}
	args = append(args, repo+"/", destination+"/")
	if out, err := exec.Command(rsync, args...).CombinedOutput(); err != nil {
		t.Fatalf("rsync: %v %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(destination, "public.txt")); err != nil {
		t.Errorf("public file not transferred: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, filepath.Base(secret))); !os.IsNotExist(err) {
		t.Errorf("secret file was transferred (stat err %v)", err)
	}
}

func TestExternalParentSourceDoesNotProduceAnInvalidRsyncParentPattern(t *testing.T) {
	repo := filepath.Join(realTempDir(t), "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := RsyncExcludes(repo, repo+"/../external.env"); len(got) != 0 {
		t.Errorf("RsyncExcludes = %q, want none", got)
	}
}

func TestSymlinkParentTraversalReadsOriginalResolvedSourceAndExcludesBothPaths(t *testing.T) {
	repo := filepath.Join(realTempDir(t), "repo")
	if err := os.MkdirAll(filepath.Join(repo, "real", "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(repo, "real", "secret.env"), "SOURCE=resolved-target\n")
	// This distinct file catches an incorrect clean-before-resolve implementation.
	write(t, filepath.Join(repo, "secret.env"), "SOURCE=lexical-parent\n")
	if err := os.Symlink(filepath.Join("real", "child"), filepath.Join(repo, "alias")); err != nil {
		t.Fatal(err)
	}
	source, err := ResolveSource(repo, repo+"/alias/../secret.env")
	if err != nil {
		t.Fatal(err)
	}
	values, err := Read(source)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(values, map[string]string{"SOURCE": "resolved-target"}) {
		t.Errorf("values = %v", values)
	}
	want := []string{"/secret.env", "/real/secret.env"}
	if !reflect.DeepEqual(source.RsyncExcludes, want) {
		t.Errorf("RsyncExcludes = %q, want %q", source.RsyncExcludes, want)
	}
}

func TestRelativeSourceResolvesFromWorkingDirectory(t *testing.T) {
	root := realTempDir(t)
	repo := filepath.Join(root, "repo")
	write(t, filepath.Join(repo, "x.env"), "A=1\n")
	chdir(t, repo)
	source, err := ResolveSource(repo, "x.env")
	if err != nil {
		t.Fatal(err)
	}
	if source.Selected != filepath.Join(repo, "x.env") || !reflect.DeepEqual(source.RsyncExcludes, []string{"/x.env"}) {
		t.Errorf("source = %+v", source)
	}
}

func TestExpandUser(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	home = strings.TrimRight(home, "/")
	cases := map[string]string{
		"~":               home,
		"~/x/y":           home + "/x/y",
		"/abs":            "/abs",
		"rel/~":           "rel/~",
		"~no-such-user-x": "~no-such-user-x",
	}
	for in, want := range cases {
		if got := ExpandUser(in); got != want {
			t.Errorf("ExpandUser(%q) = %q, want %q", in, got, want)
		}
	}
}

// Ports of the dotenv-related cases of tests/test_secrets.py.

const sentinel = "synthetic-secret-not-for-output"

func TestDotenvOverridesTomlEnvAndKeepsConfiguredFileExcludedWhenOverrideIsExternal(t *testing.T) {
	root := realTempDir(t)
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	configured := filepath.Join(repo, "credentials.prod") // deliberately absent
	external := filepath.Join(root, "override.env")
	write(t, external, "PORT=9000\nTOKEN="+sentinel+"\n")
	env, err := LoadEnvironment(repo, map[string]string{"PORT": "8000", "SAFE_FOR_SETUP": "yes"}, configured, external, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"PORT": "9000", "SAFE_FOR_SETUP": "yes", "TOKEN": sentinel}
	if !reflect.DeepEqual(env.Values, want) {
		t.Errorf("values = %v", env.Values)
	}
	if env.FileKeys != 2 || env.Source == nil || env.FromStdin {
		t.Errorf("env = %+v", env)
	}
	if !reflect.DeepEqual(env.RsyncExcludes, []string{"/credentials.prod"}) {
		t.Errorf("excludes = %q", env.RsyncExcludes)
	}
}

func TestOverrideResolvesFromCwdWhileConfiguredFileIsAlreadyAbsolute(t *testing.T) {
	root := realTempDir(t)
	repo := filepath.Join(root, "repo")
	cwd := filepath.Join(root, "cwd")
	write(t, filepath.Join(repo, "configured.env"), "SOURCE=repo-config\n")
	write(t, filepath.Join(repo, "override.env"), "SOURCE=repo-override\n")
	write(t, filepath.Join(cwd, "override.env"), "SOURCE=cwd-override\n")
	chdir(t, cwd)
	env, err := LoadEnvironment(repo, nil, filepath.Join(repo, "configured.env"), "override.env", nil)
	if err != nil {
		t.Fatal(err)
	}
	if env.Values["SOURCE"] != "cwd-override" {
		t.Errorf("SOURCE = %q", env.Values["SOURCE"])
	}
	if env.Source.Selected != filepath.Join(cwd, "override.env") {
		t.Errorf("Selected = %q", env.Source.Selected)
	}
	env, err = LoadEnvironment(repo, nil, filepath.Join(repo, "configured.env"), "", nil)
	if err != nil || env.Values["SOURCE"] != "repo-config" {
		t.Errorf("configured file: %v %v", env, err)
	}
}

func TestMissingDotenvFailsWithoutNamingTheFile(t *testing.T) {
	repo := realTempDir(t)
	_, err := LoadEnvironment(repo, nil, filepath.Join(repo, "missing.env"), "", nil)
	if err == nil || !strings.Contains(err.Error(), "not readable") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), "missing.env") {
		t.Errorf("error names the file: %q", err)
	}
}

func TestLoadEnvironmentFromStdin(t *testing.T) {
	repo := realTempDir(t)
	configured := filepath.Join(repo, ".env.prod")
	env, err := LoadEnvironment(repo, map[string]string{"A": "base", "B": "keep"}, configured, "", strings.NewReader("A="+sentinel+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !env.FromStdin || env.Source != nil || env.FileKeys != 1 {
		t.Errorf("env = %+v", env)
	}
	if env.Values["A"] != sentinel || env.Values["B"] != "keep" {
		t.Errorf("values = %v", env.Values)
	}
	if !reflect.DeepEqual(env.RsyncExcludes, []string{"/.env.prod"}) {
		t.Errorf("excludes = %q", env.RsyncExcludes)
	}
	if _, err := LoadEnvironment(repo, nil, "", "x.env", strings.NewReader("")); !errors.Is(err, ErrOverrideAndStdin) {
		t.Errorf("combined error = %v", err)
	}
	if _, err := LoadEnvironment(repo, nil, "", "", strings.NewReader("TOKEN="+sentinel+"\nbad line\n")); err == nil || strings.Contains(err.Error(), sentinel) {
		t.Errorf("stdin syntax error = %v", err)
	}
}

func TestLoadEnvironmentWithoutSourceUsesBaseOnly(t *testing.T) {
	base := map[string]string{"PORT": "8000"}
	env, err := LoadEnvironment(t.TempDir(), base, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(env.Values, base) || env.Source != nil || env.FileKeys != 0 || len(env.RsyncExcludes) != 0 {
		t.Errorf("env = %+v", env)
	}
	env.Values["PORT"] = "changed"
	if base["PORT"] != "8000" {
		t.Error("LoadEnvironment must copy the base map")
	}
}
