package transport_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/frgmt0/depper/internal/transport"
	"github.com/frgmt0/depper/internal/transport/transporttest"
)

func TestSSHOptions(t *testing.T) {
	cases := []struct {
		name   string
		target transport.Target
		want   []string
	}{
		{"minimal", transport.Target{Host: "desktop"}, []string{"-o", "BatchMode=yes"}},
		{"full", transport.Target{Host: "203.0.113.10", User: "deploy", Port: 2222, IdentityFile: "/k/id", KnownHosts: "/k/known"},
			[]string{"-p", "2222", "-i", "/k/id", "-o", "IdentitiesOnly=yes", "-l", "deploy", "-o", "UserKnownHostsFile=/k/known", "-o", "StrictHostKeyChecking=yes", "-o", "BatchMode=yes"}},
	}
	for _, tc := range cases {
		if got := tc.target.SSHOptions(); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestScriptArgsUsePOSIXShAndEndOptions(t *testing.T) {
	r := transport.NewRemote(nil, transport.Target{Host: "deploy@host.example", Port: 22})
	want := []string{"ssh", "-p", "22", "-o", "BatchMode=yes", "--", "deploy@host.example", "sh -s"}
	if got := r.ScriptArgs(); !reflect.DeepEqual(got, want) {
		t.Errorf("ScriptArgs = %q", got)
	}
}

func TestRsyncArgs(t *testing.T) {
	cases := []struct {
		name   string
		target transport.Target
		dest   string
	}{
		{"alias", transport.Target{Host: "desktop"}, "desktop:'/srv/api/releases/r1'/"},
		{"ipv4 user", transport.Target{Host: "u@203.0.113.10"}, "u@203.0.113.10:'/srv/api/releases/r1'/"},
		{"ipv6", transport.Target{Host: "2001:db8::10"}, "[2001:db8::10]:'/srv/api/releases/r1'/"},
		{"ipv6 user", transport.Target{Host: "deploy@2001:db8::10"}, "deploy@[2001:db8::10]:'/srv/api/releases/r1'/"},
	}
	for _, tc := range cases {
		r := transport.NewRemote(nil, tc.target)
		args, err := r.RsyncArgs("/repo/", "/srv/api/releases/r1/", []string{".git/", ".env", ".git/"})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		want := []string{"rsync", "-az", "-e", "ssh -o BatchMode=yes", "--exclude", ".git/", "--exclude", ".env", "/repo/", tc.dest}
		if !reflect.DeepEqual(args, want) {
			t.Errorf("%s: got %q\nwant %q", tc.name, args, want)
		}
	}
}

func TestRsyncShellQuotesForRsyncSplitter(t *testing.T) {
	target := transport.Target{Host: "h", Port: 2200, IdentityFile: "/Users/a b/it's key", KnownHosts: "/k h"}
	got := target.RsyncShell("ssh")
	want := `ssh -p 2200 -i '/Users/a b/it''s key' -o IdentitiesOnly=yes -o 'UserKnownHostsFile=/k h' -o StrictHostKeyChecking=yes -o BatchMode=yes`
	if got != want {
		t.Errorf("RsyncShell = %s\nwant          %s", got, want)
	}
}

func TestRsyncRefusesNonPlainRemotePath(t *testing.T) {
	r := transport.NewRemote(nil, transport.Target{Host: "h"})
	for _, dest := range []string{"relative", "/a b", "/a'b", "~/x", "/a%b"} {
		if _, err := r.RsyncArgs("/repo", dest, nil); err == nil {
			t.Errorf("RsyncArgs accepted %q", dest)
		}
	}
}

func TestRemoteSendsScriptsOnStdinOnly(t *testing.T) {
	rec := &transporttest.Recorder{Handler: func(c transporttest.Call) (transport.Result, error) {
		return transport.Result{Stdout: "/srv/api\n"}, nil
	}}
	r := transport.NewRemote(rec, transport.Target{Host: "desktop"})
	out, err := r.Capture(context.Background(), "set -eu\nprintf secret-not-in-argv\n")
	if err != nil || out != "/srv/api\n" {
		t.Fatalf("Capture = %q, %v", out, err)
	}
	var streamed bytes.Buffer
	if err := r.Stream(context.Background(), "journalctl\n", &streamed); err != nil {
		t.Fatal(err)
	}
	if streamed.String() != "/srv/api\n" {
		t.Errorf("streamed = %q", streamed.String())
	}
	calls := rec.Calls()
	if len(calls) != 2 || !calls[0].Capture || calls[1].Capture {
		t.Fatalf("calls = %+v", calls)
	}
	for _, c := range calls {
		if strings.Contains(strings.Join(c.Args, " "), "secret-not-in-argv") {
			t.Error("script leaked into argv")
		}
	}
	if got := rec.Scripts(); len(got) != 2 || got[0] != "set -eu\nprintf secret-not-in-argv\n" {
		t.Errorf("Scripts = %q", got)
	}
}

func TestDryRunPrintsQuotedCommandWithoutStdin(t *testing.T) {
	var out bytes.Buffer
	r := transport.NewRemote(transport.DryRun{Out: &out}, transport.Target{Host: "desktop"})
	if err := r.Run(context.Background(), "TOKEN=secret\n"); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "DRY RUN: ssh -o BatchMode=yes -- desktop 'sh -s'\n" {
		t.Errorf("dry run output = %q", got)
	}
}

func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLocalExecutor(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	ctx := context.Background()
	res, err := transport.Local{}.Run(ctx, transport.Cmd{Args: []string{"sh", "-c", "cat; echo err >&2"}, Stdin: strings.NewReader("in"), Capture: true})
	if err != nil || res.Stdout != "in" || res.Stderr != "err\n" {
		t.Fatalf("capture: %+v %v", res, err)
	}

	_, err = transport.Local{}.Run(ctx, transport.Cmd{Args: []string{"sh", "-c", "echo remote failure >&2; exit 7"}, Capture: true})
	var cerr *transport.CommandError
	if !errors.As(err, &cerr) || cerr.ExitCode != 7 || err.Error() != "remote failure" {
		t.Fatalf("captured failure: %#v", err)
	}

	var stdout, stderr bytes.Buffer
	_, err = transport.Local{}.Run(ctx, transport.Cmd{Args: []string{"sh", "-c", "echo out; echo shown >&2; exit 3"}, Stdout: &stdout, Stderr: &stderr})
	if !errors.As(err, &cerr) || !cerr.Streamed || cerr.Stderr != "shown\n" || err.Error() != "sh exited with status 3" {
		t.Fatalf("streamed failure: %#v", err)
	}
	if stdout.String() != "out\n" || stderr.String() != "shown\n" {
		t.Errorf("streams = %q / %q", stdout.String(), stderr.String())
	}

	_, err = transport.Local{}.Run(ctx, transport.Cmd{Args: []string{"depper-no-such-program-x"}})
	var nf *transport.NotFoundError
	if !errors.As(err, &nf) || err.Error() != "required program not found: depper-no-such-program-x" {
		t.Fatalf("not found: %v", err)
	}
}

func TestTailBufferKeepsLastBytes(t *testing.T) {
	tb := &transport.TailBuffer{Max: 4}
	_, _ = tb.Write([]byte("abc"))
	_, _ = tb.Write([]byte("defg"))
	if tb.String() != "defg" {
		t.Errorf("tail = %q", tb.String())
	}
}

func TestDialArgs(t *testing.T) {
	r := transport.NewRemote(nil, transport.Target{Host: "desktop", Port: 2222})
	args, err := r.DialArgs("[::1]:8080")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ssh", "-p", "2222", "-o", "BatchMode=yes", "-W", "[::1]:8080", "--", "desktop"}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("DialArgs = %q", args)
	}
	for _, bad := range []string{"noport", "-oProxy=x:80", ":80"} {
		if _, err := r.DialArgs(bad); err == nil {
			t.Errorf("DialArgs accepted %q", bad)
		}
	}
}

// fakeSSH writes a fake ssh that ignores its arguments and runs body, so the
// dialer is exercised without any network access.
func fakeSSH(t *testing.T, body string) *transport.Remote {
	t.Helper()
	path := writeScript(t, t.TempDir(), "ssh", body)
	r := transport.NewRemote(nil, transport.Target{Host: "fake-host"})
	r.SSHProgram = path
	return r
}

func TestDialContextCarriesHTTPOverStdio(t *testing.T) {
	r := fakeSSH(t, `cr=$(printf '\r')
while IFS= read -r line; do
  [ "$line" = "$cr" ] && break
  [ -z "$line" ] && break
done
printf 'HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 17\r\nConnection: close\r\n\r\n{"state":"ready"}'
`)
	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{DialContext: r.DialContext, DisableKeepAlives: true},
	}
	resp, err := client.Get("http://127.0.0.1:8080/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != `{"state":"ready"}` {
		t.Errorf("status %d body %q", resp.StatusCode, body)
	}
}

func TestDialContextReportsProhibitedForwarding(t *testing.T) {
	r := fakeSSH(t, `echo "channel 0: open failed: administratively prohibited: open failed" >&2
echo "stdio forwarding failed" >&2
exit 255
`)
	conn, err := r.DialContext(context.Background(), "tcp", "127.0.0.1:8080")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = bufio.NewReader(conn).ReadByte()
	var cerr *transport.CommandError
	if !errors.As(err, &cerr) || cerr.ExitCode != 255 {
		t.Fatalf("read error = %#v", err)
	}
	if !transport.IsForwardingProhibited(err) {
		t.Error("IsForwardingProhibited = false")
	}
}

func TestDialContextConnectionRefusedIsNotProhibited(t *testing.T) {
	r := fakeSSH(t, `echo "channel 0: open failed: connect failed: Connection refused" >&2
exit 255
`)
	conn, err := r.DialContext(context.Background(), "tcp", "127.0.0.1:8080")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = conn.Read(make([]byte, 1))
	if err == nil || transport.IsForwardingProhibited(err) {
		t.Errorf("error = %v", err)
	}
}

func TestDialContextDeadlineAndClose(t *testing.T) {
	r := fakeSSH(t, "exec sleep 30\n")
	conn, err := r.DialContext(context.Background(), "tcp", "127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	_, err = conn.Read(make([]byte, 1))
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("read error = %v", err)
	}
	start := time.Now()
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 10*time.Second {
		t.Error("Close did not reap the process")
	}
	if _, err := r.DialContext(context.Background(), "udp", "127.0.0.1:1"); err == nil {
		t.Error("udp accepted")
	}
}
