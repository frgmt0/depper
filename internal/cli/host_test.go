package cli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frgmt0/depper/internal/exitcode"
	"github.com/frgmt0/depper/internal/host"
	"github.com/frgmt0/depper/internal/transport"
	"github.com/frgmt0/depper/internal/transport/transporttest"
)

const hostTestConfig = "# app\n[service]\nname = \"app\"\ncommand = \"./run\"\n\n[deploy]\nhost = \"deploy@203.0.113.10\" # placeholder\n"

func randomBlob(t *testing.T) string {
	t.Helper()
	b := make([]byte, 51)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// hostRepo returns a repository with depper.toml and a public key file.
func hostRepo(t *testing.T, cfg string) (repo, key string) {
	t.Helper()
	repo = tempRepo(t, "repo")
	if cfg != "" {
		if err := os.WriteFile(filepath.Join(repo, "depper.toml"), []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	key = filepath.Join(filepath.Dir(repo), "id_ed25519.pub")
	if err := os.WriteFile(key, []byte("ssh-ed25519 "+randomBlob(t)+" me@laptop\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo, key
}

func setTerminal(t *testing.T, tty bool) {
	t.Helper()
	old := stdinIsTerminal
	stdinIsTerminal = func(io.Reader) bool { return tty }
	t.Cleanup(func() { stdinIsTerminal = old })
}

func TestHostInitArgumentErrors(t *testing.T) {
	repo, key := hostRepo(t, hostTestConfig)
	for _, tt := range []struct {
		args []string
		env  map[string]string
		want string
	}{
		{[]string{"host", "init", "203.0.113.7", repo, "--key", key, "--yes"}, nil, "ADMIN@HOST"},
		{[]string{"host", "init", "root@203.0.113.7", repo, "--key", key, "--yes", "--firewall", "22,http"}, nil, "invalid port"},
		{[]string{"host", "init", "root@203.0.113.7", repo, "--key", key, "--yes", "--tailscale-authkey-env", "TS_KEY"}, nil, "TS_KEY is not set"},
		{[]string{"host", "init", "root@203.0.113.7", repo, "--key", key, "--yes", "--tailscale-authkey-env", "bad-name"}, nil, "invalid environment variable"},
		{[]string{"host", "init", "root@203.0.113.7", repo, "--key", key, "--yes", "--container", "lxc"}, nil, "--container must be"},
		{[]string{"host", "init", "root@203.0.113.7", repo, "--key", key, "--yes", "--user", "Bad"}, nil, "invalid deploy user"},
		{[]string{"host", "init", "root@203.0.113.7", repo, "--key", key}, nil, "stdin is not a terminal"},
		{[]string{"host", "init", "root@203.0.113.7", repo, "--key", key, "--yes", "--target", "a.b"}, nil, "invalid target name"},
	} {
		app, _, stderr := testApp(tt.env)
		setTerminal(t, false)
		if code := Run(context.Background(), app, tt.args); code != exitcode.Failure {
			t.Errorf("%v: exit %d", tt.args, code)
		}
		if !strings.Contains(stderr.String(), tt.want) {
			t.Errorf("%v: stderr = %q, want %q", tt.args, stderr.String(), tt.want)
		}
		if calls := app.Exec.(*transporttest.Recorder).Calls(); len(calls) != 0 {
			t.Errorf("%v: ran %v", tt.args, calls)
		}
	}
}

func TestHostInitRequiresConfig(t *testing.T) {
	repo, key := hostRepo(t, "")
	app, _, stderr := testApp(nil)
	if code := Run(context.Background(), app, []string{"host", "init", "root@203.0.113.7", repo, "--key", key, "--yes"}); code != exitcode.Failure {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stderr.String(), "depper init") {
		t.Errorf("stderr = %q", stderr.String())
	}
	if len(app.Exec.(*transporttest.Recorder).Calls()) != 0 {
		t.Error("contacted the host without a config")
	}
}

// Without --container the choice is left to host.Init, which resolves the
// configuration (host.ContainerAuto); an explicit value is passed through.
func TestHostInitContainerFlag(t *testing.T) {
	repo, key := hostRepo(t, strings.Replace(hostTestConfig, "command = \"./run\"\n", "", 1)+"\n[container]\nengine = \"podman\"\nimage = \"nginx\"\n")
	for _, tt := range []struct {
		flags []string
		want  string
	}{
		{nil, host.ContainerAuto},
		{[]string{"--container", "docker"}, host.ContainerDocker},
		{[]string{"--container", "none"}, host.ContainerNone},
	} {
		app, _, _ := testApp(nil)
		cmd := newHostInitCommand(app)
		if err := cmd.ParseFlags(append([]string{"--key", key}, tt.flags...)); err != nil {
			t.Fatal(err)
		}
		var f hostInitFlags
		f.key, f.writeConfig = key, true
		if v, err := cmd.Flags().GetString("container"); err == nil {
			f.container = v
		}
		o, err := hostInitOptions(app, cmd, &f, []string{"root@203.0.113.7", repo})
		if err != nil {
			t.Fatalf("%v: %v", tt.flags, err)
		}
		if o.Container != tt.want {
			t.Errorf("%v: container = %q, want %q", tt.flags, o.Container, tt.want)
		}
	}
	app, _, stderr := testApp(nil)
	setTerminal(t, false)
	if code := Run(context.Background(), app, []string{"host", "init", "root@203.0.113.7", repo, "--key", key, "--yes", "--container", ""}); code != exitcode.Failure || !strings.Contains(stderr.String(), "--container must be") {
		t.Errorf("empty --container: exit %d, stderr %q", code, stderr.String())
	}
}

// preparedHostFacts is what the facts script reports for a host that is
// already fully prepared for user deploy.
func preparedHostFacts(t *testing.T) string {
	t.Helper()
	rec, _ := json.Marshal(host.Record{DepperVersion: "v0.2.0", CreatedAt: "2026-10-01T00:00:00Z", UpdatedAt: "2026-10-01T00:00:00Z", DeployUser: "deploy", Container: "none", Steps: []string{"user"}, Packages: []string{"rsync"}})
	lines := []string{
		"motd noise", "@@depper-host-facts", "admin_user=root", "priv=root",
		"os_ID=debian", `os_VERSION_ID="12"`, "os_VERSION_CODENAME=bookworm", `os_PRETTY_NAME="Debian GNU/Linux 12 (bookworm)"`,
		"systemd=1", "arch=x86_64", "user_exists=1", "user_uid=1000", "user_home=/home/deploy", "user_shell=/bin/bash", "user_groups=deploy",
		"ak_key=plain", "linger=1", "usermgr=1", "subuid=1", "subgid=1",
		"pkg_ca-certificates=1", "cmd_rsync=/usr/bin/rsync", "cmd_curl=/usr/bin/curl", "cmd_sha256sum=/usr/bin/sha256sum", "cmd_runuser=/usr/sbin/runuser",
		"record=" + base64.StdEncoding.EncodeToString(rec), "root_facts=1",
	}
	return strings.Join(lines, "\n") + "\n"
}

const doctorOK = "@@doctor\nuser=deploy\nbase=/home/deploy/.local/share/depper/app\nrsync=/usr/bin/rsync\nsha256sum=/usr/bin/sha256sum\nsystemctl=/usr/bin/systemctl\nsystemd=ok\nlinger=yes\ndisk_kb=99999999\nlayout=none\nlock=free\ndoctor=1\n"

func hostRecorder(t *testing.T, keyBlob string) *transporttest.Recorder {
	facts := preparedHostFacts(t)
	return &transporttest.Recorder{Handler: func(c transporttest.Call) (transport.Result, error) {
		switch {
		case c.Program() == "ssh-keyscan":
			return transport.Result{Stdout: "203.0.113.7 ssh-ed25519 " + keyBlob + "\n"}, nil
		case strings.Contains(c.Stdin, "@@depper-host-facts"):
			return transport.Result{Stdout: facts}, nil
		case strings.Contains(c.Stdin, "@@doctor"):
			return transport.Result{Stdout: doctorOK}, nil
		case strings.Contains(c.Stdin, "/etc/depper/host.json.depper-tmp"):
			// The record write (it lists the pinned host key and the
			// configuration edit).
			return transport.Result{Stdout: "@@depper-begin record\n@@depper-done record\n"}, nil
		}
		t.Errorf("unexpected command %v", c.Args)
		return transport.Result{}, nil
	}}
}

func TestHostInitJSONOnPreparedHost(t *testing.T) {
	repo, key := hostRepo(t, hostTestConfig)
	blob := randomBlob(t)
	app, stdout, stderr := testApp(nil)
	rec := hostRecorder(t, blob)
	app.Exec = rec
	fp := host.HostKey{Type: "ssh-ed25519", Blob: blob}.Fingerprint()
	code := Run(context.Background(), app, []string{"--json", "host", "init", "root@203.0.113.7", repo, "--key", key, "--yes", "--host-key-fingerprint", fp})
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	for _, key := range []string{"facts", "plan", "applied", "warnings", "doctor", "config_changes", "host_keys", "known_hosts", "next"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("JSON lacks %q", key)
		}
	}
	if doc["host_key_status"] != host.HostKeyAdded || doc["config_written"] != true || doc["next"] != "depper deploy" {
		t.Errorf("doc = %v", doc)
	}
	if !strings.Contains(stderr.String(), "SHA256:") || !strings.Contains(stderr.String(), "Host 203.0.113.7 is prepared for deploy.") {
		t.Errorf("human output not on stderr:\n%s", stderr.String())
	}
	kh := filepath.Join(repo, ".depper", "known_hosts")
	if data, err := os.ReadFile(kh); err != nil || string(data) != "203.0.113.7 ssh-ed25519 "+blob+"\n" {
		t.Errorf("known_hosts = %q %v", data, err)
	}
	got, _ := os.ReadFile(filepath.Join(repo, "depper.toml"))
	want := "# app\n[service]\nname = \"app\"\ncommand = \"./run\"\n\n[deploy]\nuser = \"deploy\"\nknown_hosts = \".depper/known_hosts\"\nhost = \"203.0.113.7\" # placeholder\n"
	if string(got) != want {
		t.Errorf("depper.toml =\n%s", got)
	}
	// The admin and deploy connections both pin the new file.
	for _, c := range rec.Calls() {
		if c.Program() == "ssh" && !strings.Contains(strings.Join(c.Args, " "), "UserKnownHostsFile="+kh+" -o StrictHostKeyChecking=yes") {
			t.Errorf("unpinned ssh: %v", c.Args)
		}
	}
}

func TestHostInitInteractiveConfirmation(t *testing.T) {
	repo, key := hostRepo(t, hostTestConfig)
	setTerminal(t, true)
	app, stdout, _ := testApp(nil)
	app.Exec = hostRecorder(t, randomBlob(t))
	app.Stdin = strings.NewReader("n\n")
	if code := Run(context.Background(), app, []string{"host", "init", "root@203.0.113.7", repo, "--key", key}); code != exitcode.Failure {
		t.Fatalf("declined run exit %d", code)
	}
	if !strings.Contains(stdout.String(), "Trust these host keys") {
		t.Errorf("no prompt:\n%s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(repo, ".depper", "known_hosts")); !os.IsNotExist(err) {
		t.Error("declined key written")
	}

	app, _, stderr := testApp(nil)
	app.Exec = hostRecorder(t, randomBlob(t))
	app.Stdin = strings.NewReader("y\ny\n")
	if code := Run(context.Background(), app, []string{"host", "init", "root@203.0.113.7", repo, "--key", key}); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
}

func TestHostInitDryRunDoesNotNeedATerminal(t *testing.T) {
	repo, key := hostRepo(t, hostTestConfig)
	setTerminal(t, false)
	app, stdout, stderr := testApp(nil)
	app.Exec = hostRecorder(t, randomBlob(t))
	if code := Run(context.Background(), app, []string{"host", "init", "root@203.0.113.7", repo, "--key", key, "--dry-run"}); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Dry run: nothing was changed.") {
		t.Errorf("stdout:\n%s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(repo, ".depper")); !os.IsNotExist(err) {
		t.Error("dry run wrote .depper/")
	}
	if got, _ := os.ReadFile(filepath.Join(repo, "depper.toml")); string(got) != hostTestConfig {
		t.Error("dry run edited depper.toml")
	}
}

func TestHostStatus(t *testing.T) {
	repo, _ := hostRepo(t, strings.Replace(hostTestConfig, "deploy@203.0.113.10", "deploy@203.0.113.7", 1))
	recJSON, _ := json.Marshal(host.Record{DepperVersion: "v0.2.0", DeployUser: "deploy", Container: "none", Hardening: host.Hardening{SSH: true}})
	out := "@@depper-host-status\nuser=deploy\nrecord=" + base64.StdEncoding.EncodeToString(recJSON) + "\nlinger=1\nusermgr=1\ncmd_rsync=/usr/bin/rsync\nstatus_done=1\n"
	app, stdout, _ := testApp(nil)
	app.Exec = &transporttest.Recorder{Handler: func(c transporttest.Call) (transport.Result, error) {
		return transport.Result{Stdout: out}, nil
	}}
	// The recorded ssh hardening cannot be confirmed (no sshd -T as the
	// deploy user): a warning, not a failure.
	if code := Run(context.Background(), app, []string{"--json", "host", "status", repo}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var doc host.StatusResult
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	statuses := map[string]string{}
	for _, c := range doc.Checks {
		statuses[c.Name] = c.Status
	}
	if statuses["ssh hardening"] != host.StatusWarn || statuses["linger"] != host.StatusPass || statuses["firewall"] != host.StatusInfo || doc.Record == nil {
		t.Errorf("status = %+v", doc)
	}
	calls := app.Exec.(*transporttest.Recorder).Calls()
	if len(calls) != 1 || !strings.Contains(strings.Join(calls[0].Args, " "), "-- deploy@203.0.113.7 sh -s") {
		t.Errorf("status did not connect as the deploy user: %v", calls)
	}
}

func TestHostInitRepoFirstForm(t *testing.T) {
	repo, key := hostRepo(t, hostTestConfig)
	app, _, stderr := testApp(nil)
	app.Exec = hostRecorder(t, randomBlob(t))
	if code := Run(context.Background(), app, []string{repo, "host", "init", "root@203.0.113.7", "--key", key, "--yes", "--trust-on-first-use"}); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if got, _ := os.ReadFile(filepath.Join(repo, "depper.toml")); !strings.Contains(string(got), `host = "203.0.113.7"`) {
		t.Errorf("depper.toml = %s", got)
	}
}
