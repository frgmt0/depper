package tunnel

// Test harness: a fake "remote host" in a temporary directory. Scripts sent
// to ssh run locally with dash (or sh) -s, with HOME pointing at the fake
// remote home and PATH of only fake systemctl, journalctl, and
// cloudflared programs plus allowlisted basic utilities (isolation_test.go); rsync copies locally. Nothing contacts a real host
// or Cloudflare.

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frgmt0/depper/internal/config"
	"github.com/frgmt0/depper/internal/engine"
	"github.com/frgmt0/depper/internal/transport"
)

const fakeSystemctl = `#!/bin/sh
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
      for p in $props; do
        case "$p" in
          LoadState) echo "LoadState=$load" ;;
          ActiveState) echo "ActiveState=$active" ;;
          SubState) echo "SubState=$sub" ;;
          MainPID) echo "MainPID=$pid" ;;
          NRestarts) echo "NRestarts=0" ;;
          ActiveEnterTimestamp) echo "ActiveEnterTimestamp=$since" ;;
        esac
      done
    done ;;
  status) for u in $units; do echo "fake status of $u"; done ;;
  *) echo "fake systemctl: unsupported $cmd" >&2; exit 1 ;;
esac
`

const fakeJournalctl = `#!/bin/sh
printf '%s\n' "journalctl $*" >> "$FAKE_ROOT/journalctl.log"
echo "journal line for $*"
`

// fakeCloudflared keeps its tunnels as "id name" lines in
// $FAKE_ROOT/cf-tunnels. A tunnel lists a connection while a started
// connector unit runs it. Flag files: cf-next-id (id for create),
// cf-create-unlisted (create does not show up in list), cf-route-fail,
// cf-no-connect, cf-list-null (an empty list prints null).
const fakeCloudflared = `#!/bin/sh
set -eu
printf '%s|force=%s\n' "cloudflared $*" "${TUNNEL_FORCE_PROVISIONING_DNS-unset}" >> "$FAKE_ROOT/cloudflared.log"
db="$FAKE_ROOT/cf-tunnels"
touch "$db"
config=""
if test "$1" = --config; then config=$2; shift 2; fi
test "$1" = tunnel || { echo "fake cloudflared: unsupported $*" >&2; exit 2; }
shift
connected() {
  test -f "$FAKE_ROOT/cf-no-connect" && return 1
  for f in "$FAKE_ROOT"/systemd/*tunnel@*.service.active; do
    test -f "$f" || continue
    grep -q "tunnel run $1\$" "${f%.active}.started-unit" 2>/dev/null && return 0
  done
  return 1
}
case "$1" in
  list)
    if test -f "$FAKE_ROOT/cf-list-null" && ! test -s "$db"; then echo null; exit 0; fi
    printf '['
    sep=''
    while read -r id name; do
      conns='[]'
      if connected "$id"; then conns='[{"colo_name":"ams01","id":"c1","is_pending_reconnect":false}]'; fi
      printf '%s{"id":"%s","name":"%s","created_at":"2026-10-12T10:00:00Z","deleted_at":"0001-01-01T00:00:00Z","connections":%s}' "$sep" "$id" "$name" "$conns"
      sep=','
    done < "$db"
    printf ']\n' ;;
  create)
    name=$2
    if grep -q " $name\$" "$db"; then echo "tunnel with name already exists" >&2; exit 1; fi
    id=$(cat "$FAKE_ROOT/cf-next-id")
    mkdir -p "$HOME/.cloudflared"
    printf '{"AccountTag":"a","TunnelSecret":"CREDENTIAL-SECRET-VALUE","TunnelID":"%s"}\n' "$id" > "$HOME/.cloudflared/$id.json"
    if ! test -f "$FAKE_ROOT/cf-create-unlisted"; then printf '%s %s\n' "$id" "$name" >> "$db"; fi
    echo "Tunnel credentials written to $HOME/.cloudflared/$id.json: CREDENTIAL-SECRET-VALUE"
    echo "Created tunnel $name with id $id" ;;
  route)
    test "$2" = dns || exit 2
    if test -f "$FAKE_ROOT/cf-route-fail"; then echo "Failed to add route: code: 1003, reason: An A, AAAA, or CNAME record with that host already exists." >&2; exit 1; fi
    printf '%s %s %s\n' "$3" "$4" "$5" >> "$FAKE_ROOT/cf-routes"
    echo "Added CNAME $5 which will route to this tunnel" ;;
  ingress)
    test "$2" = validate || exit 2
    grep -q '"ingress"' "$config" || { echo "invalid ingress" >&2; exit 1; }
    echo "Validating rules from $config"
    echo OK ;;
  *) echo "fake cloudflared: unsupported tunnel $1" >&2; exit 2 ;;
esac
`

// mvShim adds GNU mv -T on systems whose mv lacks it (macOS).
const mvShim = `#!/bin/sh
case "$1" in
  -Tf|-fT) shift; exec perl -e 'rename($ARGV[0], $ARGV[1]) or die "mv: $!\n"' "$1" "$2" ;;
esac
exec /bin/mv "$@"
`

const sha256Shim = `#!/bin/sh
sum=$(shasum -a 256 | cut -d' ' -f1)
printf '%s  -\n' "$sum"
`

const tunnelID = "6f1d3c1e-2b9a-4c55-9a51-0d2f2e7f3a10"

// fakeHost is a fake remote host and a transport.Executor.
type fakeHost struct {
	t     *testing.T
	Root  string
	Home  string
	Out   *bytes.Buffer
	shell string

	mu      sync.Mutex
	scripts []string
}

func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := &fakeHost{t: t, Root: root, Home: filepath.Join(root, "home"), Out: &bytes.Buffer{}, shell: "sh"}
	if p, err := exec.LookPath("dash"); err == nil {
		h.shell = p
	}
	for _, dir := range []string{"home", "remote-bin", "run", "tmp"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	setupSysBin(t, root)
	h.writeExec("remote-bin/systemctl", fakeSystemctl)
	h.writeExec("remote-bin/journalctl", fakeJournalctl)
	h.writeExec("remote-bin/cloudflared", fakeCloudflared)
	if out, err := exec.Command("mv", "--version").CombinedOutput(); err != nil || !strings.Contains(string(out), "GNU") {
		if _, err := exec.LookPath("perl"); err != nil {
			t.Skip("neither GNU mv nor perl is available for mv -T")
		}
		h.writeExec("remote-bin/mv", mvShim)
	}
	if _, err := exec.LookPath("sha256sum"); err != nil {
		h.writeExec("remote-bin/sha256sum", sha256Shim)
	}
	h.writeFile("cf-next-id", tunnelID+"\n")
	return h
}

func (h *fakeHost) writeExec(rel, body string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.Root, rel), []byte(body), 0o755); err != nil {
		h.t.Fatal(err)
	}
}

// writeFile writes a file relative to the fake root.
func (h *fakeHost) writeFile(rel, body string) {
	h.t.Helper()
	path := filepath.Join(h.Root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

// writeHome writes a file relative to the fake remote home.
func (h *fakeHost) writeHome(rel, body string) { h.writeFile(filepath.Join("home", rel), body) }

func (h *fakeHost) readHome(rel string) string {
	h.t.Helper()
	data, err := os.ReadFile(filepath.Join(h.Home, rel))
	if err != nil {
		h.t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

func (h *fakeHost) existsHome(rel string) bool {
	_, err := os.Lstat(filepath.Join(h.Home, rel))
	return err == nil
}

func (h *fakeHost) exists(rel string) bool {
	_, err := os.Lstat(filepath.Join(h.Root, rel))
	return err == nil
}

func (h *fakeHost) log(name string) string {
	data, _ := os.ReadFile(filepath.Join(h.Root, name))
	return string(data)
}

func (h *fakeHost) Scripts() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.scripts...)
}

// Run implements transport.Executor.
func (h *fakeHost) Run(ctx context.Context, c transport.Cmd) (transport.Result, error) {
	switch c.Args[0] {
	case "ssh":
		if c.Args[len(c.Args)-1] != transport.RemoteShell {
			h.t.Errorf("unexpected ssh command %q", c.Args)
			return transport.Result{}, &transport.CommandError{Program: "ssh", ExitCode: 255}
		}
		data, err := io.ReadAll(c.Stdin)
		if err != nil {
			return transport.Result{}, err
		}
		h.mu.Lock()
		h.scripts = append(h.scripts, string(data))
		h.mu.Unlock()
		env := []string{
			"HOME=" + h.Home,
			"PATH=" + remotePATH(h.Root),
			"FAKE_ROOT=" + h.Root,
			"XDG_RUNTIME_DIR=" + filepath.Join(h.Root, "run"),
			"TMPDIR=" + filepath.Join(h.Root, "tmp"),
			// An inherited force flag that depper must clear.
			"TUNNEL_FORCE_PROVISIONING_DNS=true",
		}
		// Root/login-noise emulates a login shell printing text first.
		noise, _ := os.ReadFile(filepath.Join(h.Root, "login-noise"))
		if !c.Capture && c.Stdout != nil && len(noise) > 0 {
			_, _ = c.Stdout.Write(noise)
		}
		res, err := transport.Local{}.Run(ctx, transport.Cmd{Args: []string{h.shell, "-s"}, Stdin: bytes.NewReader(data), Capture: c.Capture, Stdout: c.Stdout, Stderr: c.Stderr, Dir: h.Home, Env: env})
		if c.Capture {
			res.Stdout = string(noise) + res.Stdout
		}
		return res, err
	case "rsync":
		args := []string{"rsync", "-a"}
		var positional []string
		for i := 1; i < len(c.Args); i++ {
			switch a := c.Args[i]; a {
			case "-e":
				i++
			case "--exclude":
				args = append(args, "--exclude", c.Args[i+1])
				i++
			case "-az":
			default:
				positional = append(positional, a)
			}
		}
		if len(positional) != 2 {
			h.t.Errorf("unexpected rsync %q", c.Args)
			return transport.Result{}, &transport.CommandError{Program: "rsync", ExitCode: 2}
		}
		// host:/abs/path/, never quoted (rsync >= 3.2.4 keeps quotes literally).
		dest := positional[1]
		start := strings.Index(dest, ":/")
		if start < 0 || !strings.HasSuffix(dest, "/") || strings.ContainsAny(dest, "'\"\\ ") {
			h.t.Errorf("unexpected rsync destination %q", dest)
			return transport.Result{}, &transport.CommandError{Program: "rsync", ExitCode: 2}
		}
		args = append(args, positional[0], dest[start+1:])
		return transport.Local{}.Run(ctx, transport.Cmd{Args: args, Capture: c.Capture, Stdout: c.Stdout, Stderr: c.Stderr})
	}
	return transport.Local{}.Run(ctx, c)
}

const baseRel = ".local/share/depper/api"

const tunnelToml = `[service]
name = "api"
command = "python3 app.py"

[deploy]
host = "deploy@fakehost"

[tunnel]
hostname = "api.demo.example.com"
service = "http://127.0.0.1:8080"
`

func writeRepo(t *testing.T, toml string) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"depper.toml": toml, "app.py": "print('hello')\n"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

func loadConfig(t *testing.T, toml string) *config.Config {
	t.Helper()
	cfg, err := config.LoadTarget(writeRepo(t, toml), "", func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// withoutTunnel removes the [tunnel] section.
func withoutTunnel(toml string) string {
	before, _, _ := strings.Cut(toml, "[tunnel]")
	return before
}

// sleeps records engine sleeps without waiting.
type sleeps struct {
	mu sync.Mutex
	d  []time.Duration
}

func (s *sleeps) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.d = append(s.d, d)
	s.mu.Unlock()
	return ctx.Err()
}

// Engine returns an engine for toml wired to the fake host.
func (h *fakeHost) Engine(toml string) (*engine.Engine, *sleeps) {
	cfg := loadConfig(h.t, toml)
	remote := transport.NewRemote(h, cfg.SSHTarget())
	remote.Stdout, remote.Stderr = h.Out, h.Out
	e := engine.New(cfg, remote, h, h.Out)
	s := &sleeps{}
	e.Sleep = s.sleep
	e.Settle = time.Millisecond
	e.Holder = "tester@laptop"
	return e, s
}

// dns is a fake resolver: names in resolves answer with an address, every
// other name is NXDOMAIN; err (when set) is returned for every lookup.
type dns struct {
	mu       sync.Mutex
	resolves map[string]bool
	err      error
	lookups  []string
}

func (d *dns) lookup(_ context.Context, host string) ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lookups = append(d.lookups, host)
	if d.err != nil {
		return nil, d.err
	}
	if d.resolves[host] {
		return []string{"203.0.113.7"}, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func newManager() (*Manager, *dns) {
	d := &dns{resolves: map[string]bool{}}
	return &Manager{LookupHost: d.lookup}, d
}

func ownedState(name string) string {
	return `{"hostname": "api.demo.example.com", "id": "` + tunnelID + `", "name": "` + name + `", "service": "http://127.0.0.1:8080"}` + "\n"
}
