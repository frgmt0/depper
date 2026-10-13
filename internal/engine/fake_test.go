package engine

// Test harness: a fake "remote host" in a temporary directory.
//
// The test binary doubles as the fake ssh and rsync programs (re-executed
// through small wrapper scripts with DEPPER_FAKE set). Fake ssh runs the
// script it receives with dash (or sh) -s, with HOME pointing at the fake
// remote home and a PATH of only the fake tools plus an allowlist of basic
// utilities (see isolation_test.go);
// `ssh -W` serves canned HTTP responses. Fake rsync copies with the local
// rsync. Nothing ever contacts a real host.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frgmt0/depper/internal/config"
	"github.com/frgmt0/depper/internal/transport"
)

func TestMain(m *testing.M) {
	switch os.Getenv("DEPPER_FAKE") {
	case "ssh":
		os.Exit(fakeSSHMain(os.Args[1:]))
	case "rsync":
		os.Exit(fakeRsyncMain(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func fakeSSHMain(args []string) int {
	root := os.Getenv("FAKE_ROOT")
	forward := ""
	i := 0
	for i < len(args) {
		switch args[i] {
		case "-W":
			forward = args[i+1]
			i += 2
		case "-p", "-i", "-l", "-o":
			i += 2
		case "--":
			i++
			goto host
		default:
			i++
		}
	}
host:
	if i >= len(args) {
		fmt.Fprintln(os.Stderr, "fake ssh: missing host")
		return 255
	}
	rest := args[i+1:]
	if forward != "" {
		return fakeForward(root, forward)
	}
	if strings.Join(rest, " ") != transport.RemoteShell {
		fmt.Fprintf(os.Stderr, "fake ssh: unexpected remote command %q\n", rest)
		return 255
	}
	shellProgram := "sh"
	if p, err := exec.LookPath("dash"); err == nil {
		shellProgram = p
	}
	// FAKE_ROOT/login-noise emulates a login shell that prints text (motd,
	// rc files) before the script runs.
	if noise, err := os.ReadFile(filepath.Join(root, "login-noise")); err == nil {
		_, _ = os.Stdout.Write(noise)
	}
	cmd := exec.Command(shellProgram, "-s")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Dir = filepath.Join(root, "home")
	env := []string{
		"HOME=" + filepath.Join(root, "home"),
		"PATH=" + remotePATH(root),
		"FAKE_ROOT=" + root,
		"XDG_RUNTIME_DIR=" + filepath.Join(root, "run"),
		"TMPDIR=" + filepath.Join(root, "tmp"),
	}
	// FAKE_ROOT/login-xdg emulates a login that sets another user's
	// XDG_RUNTIME_DIR (pam_env with /etc/environment).
	if xdg, err := os.ReadFile(filepath.Join(root, "login-xdg")); err == nil {
		env = append(env, "XDG_RUNTIME_DIR="+strings.TrimSpace(string(xdg)))
	}
	cmd.Env = env
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode()
		}
		fmt.Fprintln(os.Stderr, "fake ssh:", err)
		return 255
	}
	return 0
}

// fakeForward emulates `ssh -W`: FAKE_ROOT/forward-prohibited refuses
// forwarding; FAKE_ROOT/http-status (+ http-body) answers one HTTP request;
// otherwise the connection is refused.
func fakeForward(root, addr string) int {
	// Real ssh prints this (LocalCommand) once connected and authenticated;
	// FAKE_ROOT/forward-delay (milliseconds) emulates a slow handshake.
	if raw, err := os.ReadFile(filepath.Join(root, "forward-delay")); err == nil {
		ms, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
		time.Sleep(time.Duration(ms) * time.Millisecond)
	}
	fmt.Fprintln(os.Stderr, transport.ForwardReadyMarker)
	if _, err := os.Stat(filepath.Join(root, "forward-prohibited")); err == nil {
		fmt.Fprintln(os.Stderr, "channel 0: open failed: administratively prohibited: open failed")
		fmt.Fprintln(os.Stderr, "stdio forwarding failed")
		return 255
	}
	statusRaw, err := os.ReadFile(filepath.Join(root, "http-status"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "channel 0: open failed: connect failed: Connection refused")
		fmt.Fprintln(os.Stderr, "stdio forwarding failed")
		return 255
	}
	req, err := http.ReadRequest(bufio.NewReader(os.Stdin))
	if err != nil {
		return 255
	}
	_ = os.WriteFile(filepath.Join(root, "http-last-request"), []byte(addr+" "+req.Method+" "+req.URL.Path), 0o644)
	// FAKE_ROOT/response-delay (milliseconds) emulates a slow application.
	if raw, err := os.ReadFile(filepath.Join(root, "response-delay")); err == nil {
		ms, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
		time.Sleep(time.Duration(ms) * time.Millisecond)
	}
	body, _ := os.ReadFile(filepath.Join(root, "http-body"))
	status, _ := strconv.Atoi(strings.TrimSpace(string(statusRaw)))
	fmt.Fprintf(os.Stdout, "HTTP/1.1 %d %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", status, http.StatusText(status), len(body), body)
	return 0
}

func fakeRsyncMain(args []string) int {
	var out []string
	out = append(out, "-a")
	var positional []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-e":
			i++
		case a == "--exclude":
			out = append(out, "--exclude", args[i+1])
			i++
		case a == "-az":
		default:
			positional = append(positional, a)
		}
	}
	if len(positional) != 2 {
		fmt.Fprintf(os.Stderr, "fake rsync: unexpected arguments %q\n", args)
		return 2
	}
	// The destination must be host:/abs/path/ with nothing quoted: rsync
	// >= 3.2.4 would keep quotes as literal path characters.
	dest := positional[1]
	colon := strings.Index(dest, ":/")
	if colon < 0 || !strings.HasSuffix(dest, "/") || strings.ContainsAny(dest, "'\"\\ ") {
		fmt.Fprintf(os.Stderr, "fake rsync: unexpected destination %q\n", dest)
		return 2
	}
	local := dest[colon+1:]
	_ = os.WriteFile(filepath.Join(os.Getenv("FAKE_ROOT"), "rsync-args"), []byte(strings.Join(args, "\n")), 0o644)
	cmd := exec.Command("rsync", append(out, positional[0], local)...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "fake rsync:", err)
		return 1
	}
	return 0
}

const fakeSystemctl = `#!/bin/sh
if test -f "$FAKE_ROOT/fail-systemctl"; then pat=$(cat "$FAKE_ROOT/fail-systemctl"); case "$*" in $pat) echo "injected systemctl failure" >&2; exit 1 ;; esac; fi
state="$FAKE_ROOT/systemd"
mkdir -p "$state"
printf '%s\n' "systemctl $*" >> "$FAKE_ROOT/systemctl.log"
test "$1" = --user && shift
cmd=$1; shift
quiet=no; now=no
units=""
props=""
while test $# -gt 0; do
  case "$1" in
    --quiet) quiet=yes ;;
    --now) now=yes ;;
    --no-pager|--value) ;;
    -p) props="$props $2"; shift ;;
    *) units="$units $1" ;;
  esac
  shift
done
unitfile() { printf '%s' "$HOME/.config/systemd/user/$1"; }
case "$cmd" in
  daemon-reload) exit 0 ;;
  enable)
    for u in $units; do : > "$state/$u.enabled"; if test $now = yes; then : > "$state/$u.active"; fi; done ;;
  disable)
    for u in $units; do rm -f "$state/$u.enabled"; if test $now = yes; then rm -f "$state/$u.active"; fi; done ;;
  restart|start)
    for u in $units; do
      test -f "$(unitfile "$u")" || { echo "Unit $u not found." >&2; exit 5; }
      if test -f "$state/$u.crash"; then rm -f "$state/$u.active"; : > "$state/$u.failed"; else rm -f "$state/$u.failed"; : > "$state/$u.active"; fi
      cp "$(unitfile "$u")" "$state/$u.started-unit"
    done ;;
  stop)
    for u in $units; do rm -f "$state/$u.active"; done ;;
  is-active)
    for u in $units; do test -f "$state/$u.active" || exit 3; done ;;
  is-enabled)
    for u in $units; do test -f "$state/$u.enabled" || exit 1; done ;;
  show)
    for u in $units; do
      if test -f "$(unitfile "$u")"; then load=loaded; else load=not-found; fi
      if test -f "$state/$u.active"; then active=active; sub=running; pid=4242; since="Mon 2026-10-12 10:00:00 UTC"
      elif test -f "$state/$u.failed"; then active=failed; sub=failed; pid=0; since=""
      else active=inactive; sub=dead; pid=0; since=""; fi
      restarts=0; if test -f "$state/$u.nrestarts"; then restarts=$(cat "$state/$u.nrestarts"); fi
      for p in $props; do
        case "$p" in
          LoadState) echo "LoadState=$load" ;;
          ActiveState) echo "ActiveState=$active" ;;
          SubState) echo "SubState=$sub" ;;
          MainPID) echo "MainPID=$pid" ;;
          NRestarts) echo "NRestarts=$restarts" ;;
          ActiveEnterTimestamp) echo "ActiveEnterTimestamp=$since" ;;
        esac
      done
    done ;;
  status) echo "fake status" ;;
  *) echo "fake systemctl: unsupported $cmd" >&2; exit 1 ;;
esac
`

const fakeJournalctl = `#!/bin/sh
printf '%s\n' "journalctl $*" >> "$FAKE_ROOT/journalctl.log"
echo "journal line for $*"
`

// fakeDocker records its arguments and keeps an image list: docker-images
// holds one "ref label" line per image (label "-" when unlabelled).
const fakeDocker = `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_ROOT/docker.log"
printf '%s\n' "${XDG_RUNTIME_DIR:-}" >> "$FAKE_ROOT/docker-xdg.log"
images="$FAKE_ROOT/docker-images"
touch "$images"
case "$1" in
  images)
    shift; label=""
    while test $# -gt 0; do case "$1" in --filter) case "$2" in label=*) label=${2#label=} ;; esac; shift ;; --format) shift ;; esac; shift; done
    while read -r ref lab; do
      if test -z "$label" || test "$lab" = "$label"; then printf '%s\n' "$ref"; fi
    done < "$images" ;;
  build)
    shift; tag=""; lab="-"
    while test $# -gt 0; do case "$1" in -t) tag=$2; shift ;; --label) lab=$2; shift ;; esac; shift; done
    printf '%s %s\n' "$tag" "$lab" >> "$images" ;;
  pull) ;;
  tag) printf '%s -\n' "$3" >> "$images" ;;
  image)
    if test "$2" = inspect && test -f "$FAKE_ROOT/image-digests"; then cat "$FAKE_ROOT/image-digests"; fi ;;
  rmi)
    if ! grep -q "^$2 " "$images"; then echo "No such image: $2" >&2; exit 1; fi
    grep -v "^$2 " "$images" > "$images.next" || true
    mv "$images.next" "$images" ;;
esac
exit 0
`

// mvShim adds GNU mv -T on systems whose mv lacks it (macOS): rename(2)
// replaces a symlink atomically without following it.
const mvShim = `#!/bin/sh
case "$1" in
  -T|-Tf|-fT) shift; exec perl -e 'rename($ARGV[0], $ARGV[1]) or die "mv: $!\n"' "$1" "$2" ;;
esac
exec /bin/mv "$@"
`

const sha256Shim = `#!/bin/sh
sum=$(shasum -a 256 | cut -d' ' -f1)
printf '%s  -\n' "$sum"
`

// fakeHost is a fake remote host rooted in a temporary directory.
type fakeHost struct {
	t     *testing.T
	Root  string
	Home  string
	Exec  *spyExec
	Out   *syncBuffer
	ssh   string
	rsync string
}

func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync not available")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := &fakeHost{t: t, Root: root, Home: filepath.Join(root, "home"), Out: &syncBuffer{}}
	for _, dir := range []string{"home", "remote-bin", "run", "tmp", "bin"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	setupSysBin(t, root)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	h.ssh = h.writeExec("bin/ssh", "#!/bin/sh\nDEPPER_FAKE=ssh exec "+shellWord(self)+" \"$@\"\n")
	h.rsync = h.writeExec("bin/rsync", "#!/bin/sh\nDEPPER_FAKE=rsync exec "+shellWord(self)+" \"$@\"\n")
	h.writeExec("remote-bin/systemctl", fakeSystemctl)
	h.writeExec("remote-bin/journalctl", fakeJournalctl)
	h.writeExec("remote-bin/docker", fakeDocker)
	if out, err := exec.Command("mv", "--version").CombinedOutput(); err != nil || !strings.Contains(string(out), "GNU") {
		if _, err := exec.LookPath("perl"); err != nil {
			t.Skip("neither GNU mv nor perl is available for mv -T")
		}
		h.writeExec("remote-bin/mv", mvShim)
	}
	if _, err := exec.LookPath("sha256sum"); err != nil {
		h.writeExec("remote-bin/sha256sum", sha256Shim)
	}
	t.Setenv("FAKE_ROOT", root)
	h.Exec = &spyExec{inner: transport.Local{}}
	return h
}

func shellWord(s string) string { return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'" }

func (h *fakeHost) writeExec(rel, body string) string {
	h.t.Helper()
	path := filepath.Join(h.Root, rel)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		h.t.Fatal(err)
	}
	return path
}

// Engine returns an engine for cfg wired to the fake host.
func (h *fakeHost) Engine(cfg *config.Config) *Engine {
	remote := transport.NewRemote(h.Exec, cfg.SSHTarget())
	remote.SSHProgram, remote.RsyncProgram = h.ssh, h.rsync
	remote.Stdout, remote.Stderr = h.Out, h.Out
	e := New(cfg, remote, h.Exec, h.Out)
	e.Settle = 10 * time.Millisecond
	e.Holder = "tester@laptop"
	return e
}

func (h *fakeHost) path(rel string) string { return filepath.Join(h.Home, rel) }

func (h *fakeHost) read(rel string) string {
	h.t.Helper()
	data, err := os.ReadFile(h.path(rel))
	if err != nil {
		h.t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

func (h *fakeHost) exists(rel string) bool {
	_, err := os.Lstat(h.path(rel))
	return err == nil
}

func (h *fakeHost) link(rel string) string {
	h.t.Helper()
	target, err := os.Readlink(h.path(rel))
	if err != nil {
		h.t.Fatalf("readlink %s: %v", rel, err)
	}
	return target
}

func (h *fakeHost) mode(rel string) os.FileMode {
	h.t.Helper()
	info, err := os.Lstat(h.path(rel))
	if err != nil {
		h.t.Fatalf("stat %s: %v", rel, err)
	}
	return info.Mode().Perm()
}

func (h *fakeHost) touch(rel string) {
	h.t.Helper()
	if err := os.MkdirAll(filepath.Dir(h.path(rel)), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(h.path(rel), nil, 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *fakeHost) log(name string) string {
	data, _ := os.ReadFile(filepath.Join(h.Root, name))
	return string(data)
}

// spyExec records every command's argv and stdin, then delegates.
type spyExec struct {
	inner transport.Executor
	mu    sync.Mutex
	calls []transport.Cmd
	stdin []string
}

func (s *spyExec) Run(ctx context.Context, c transport.Cmd) (transport.Result, error) {
	input := ""
	if c.Stdin != nil {
		data, err := io.ReadAll(c.Stdin)
		if err != nil {
			return transport.Result{}, err
		}
		input = string(data)
		c.Stdin = strings.NewReader(input)
	}
	s.mu.Lock()
	s.calls = append(s.calls, transport.Cmd{Args: append([]string(nil), c.Args...)})
	s.stdin = append(s.stdin, input)
	s.mu.Unlock()
	return s.inner.Run(ctx, c)
}

// Scripts returns the stdin of every ssh script call.
func (s *spyExec) Scripts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for i, c := range s.calls {
		if len(c.Args) > 0 && c.Args[len(c.Args)-1] == transport.RemoteShell {
			out = append(out, s.stdin[i])
		}
	}
	return out
}

// Argv returns every recorded argv joined by spaces.
func (s *spyExec) Argv() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, c := range s.calls {
		out = append(out, strings.Join(c.Args, " "))
	}
	return out
}

// checkScriptSyntax runs `sh -n` and, when available, `dash -n` and
// checkbashisms on every script.
func checkScriptSyntax(t *testing.T, scripts []string) {
	t.Helper()
	checkers := [][]string{{"sh", "-n"}}
	if p, err := exec.LookPath("dash"); err == nil {
		checkers = append(checkers, []string{p, "-n"})
	}
	if p, err := exec.LookPath("checkbashisms"); err == nil {
		checkers = append(checkers, []string{p, "-"})
	}
	for i, script := range scripts {
		for _, c := range checkers {
			cmd := exec.Command(c[0], c[1:]...)
			cmd.Stdin = strings.NewReader(script)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("%v rejected script %d: %v\n%s\n%s", c, i, err, out, script)
			}
		}
	}
}

// writeRepo creates a repository with depper.toml and files.
func writeRepo(t *testing.T, toml string, files map[string]string) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "depper.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		writeFile(t, filepath.Join(repo, name), body)
	}
	return repo
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func loadConfig(t *testing.T, repo string) *config.Config {
	t.Helper()
	cfg, err := config.LoadTarget(repo, "", func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// syncBuffer is a bytes.Buffer safe for the concurrent stdout and stderr
// copies of one command (and the lock refresher).
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}
