package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/frgmt0/depper/internal/engine"
	"github.com/frgmt0/depper/internal/exitcode"
	"github.com/frgmt0/depper/internal/transport"
	"github.com/frgmt0/depper/internal/transport/transporttest"
	"github.com/frgmt0/depper/internal/tunnel"
)

const cliTunnelID = "6f1d3c1e-2b9a-4c55-9a51-0d2f2e7f3a10"

const cliTunnelToml = cliToml + "[tunnel]\nhostname = \"api.demo.example.com\"\nservice = \"http://127.0.0.1:8080\"\n"

const cliTunnelState = `{"hostname":"api.demo.example.com","id":"` + cliTunnelID + `","name":"depper-api","service":"http://127.0.0.1:8080"}`

// tunnelRemote answers the tunnel scripts: the owned state (state, "" for
// none), the cloudflared lookup, the lock, and a connected tunnel listing.
func tunnelRemote(state string) func(transporttest.Call) (transport.Result, error) {
	return fakeRemote("depper", func(c transporttest.Call) (transport.Result, error) {
		switch {
		case strings.Contains(c.Stdin, `printf '@@cloudflared\n'`):
			return transport.Result{Stdout: "@@cloudflared\n/usr/bin/cloudflared\n@@state\n" + state}, nil
		case strings.Contains(c.Stdin, `printf '@@state\n'`):
			return transport.Result{Stdout: "@@state\n" + state}, nil
		case strings.Contains(c.Stdin, `if mkdir "$lock"`):
			return transport.Result{Stdout: "acquired\n"}, nil
		case strings.Contains(c.Stdin, "tunnel list --output json"):
			return transport.Result{Stdout: `[{"id":"` + cliTunnelID + `","name":"depper-api","connections":[{"id":"c"}]}]`}, nil
		}
		return transport.Result{}, nil
	})
}

func TestTunnelIsRegisteredWithTheEngine(t *testing.T) {
	if engine.RegisteredTunnel() == nil {
		t.Fatal("the CLI does not link the tunnel implementation")
	}
}

func TestTunnelCommandEnsuresUnderTheLock(t *testing.T) {
	saved := tunnel.Default.ConnectSettle
	tunnel.Default.ConnectSettle = time.Nanosecond
	t.Cleanup(func() { tunnel.Default.ConnectSettle = saved })

	repo := repoWith(t, cliTunnelToml, nil)
	app, stdout, stderr := testApp(nil)
	recorderOf(app).Handler = tunnelRemote(cliTunnelState)
	if code := Run(context.Background(), app, []string{"--json", "tunnel", repo}); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var out map[string]string
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("stdout %q: %v", stdout.String(), err)
	}
	if out["id"] != cliTunnelID || out["url"] != "https://api.demo.example.com" || out["unit"] != "depper-tunnel@api.service" {
		t.Errorf("json = %v", out)
	}
	scripts := recorderOf(app).Scripts()
	joined := strings.Join(scripts, "\n")
	lock, unlock := -1, -1
	for i, s := range scripts {
		if strings.Contains(s, `if mkdir "$lock"`) {
			lock = i
		}
		if strings.Contains(s, `rmdir "$lock"`) {
			unlock = i
		}
	}
	if lock < 0 || unlock != len(scripts)-1 {
		t.Errorf("lock %d unlock %d of %d scripts", lock, unlock, len(scripts))
	}
	for _, want := range []string{"tunnel route dns --overwrite-dns=false " + cliTunnelID + " api.demo.example.com", "systemctl --user restart depper-tunnel@api.service"} {
		if !strings.Contains(joined, want) {
			t.Errorf("scripts lack %q", want)
		}
	}
	if strings.Contains(joined, "tunnel create") {
		t.Error("an owned tunnel was created again")
	}

	// Without [tunnel] the command fails before contacting the host.
	plain := repoWith(t, cliToml, nil)
	app, _, stderr = testApp(nil)
	if code := Run(context.Background(), app, []string{"tunnel", plain}); code != exitcode.Failure || !strings.Contains(stderr.String(), "[tunnel] is required") || len(recorderOf(app).Calls()) != 0 {
		t.Errorf("exit %d stderr %q calls %d", code, stderr.String(), len(recorderOf(app).Calls()))
	}
}

func TestTunnelStatusAndLogs(t *testing.T) {
	repo := repoWith(t, cliTunnelToml, nil)
	app, stdout, stderr := testApp(nil)
	recorderOf(app).Handler = fakeRemote("depper", func(c transporttest.Call) (transport.Result, error) {
		return transport.Result{Stdout: "unit output\n"}, nil
	})
	// The Python CLI's `REPO COMMAND` order still works.
	if code := Run(context.Background(), app, []string{repo, "tunnel-status"}); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	scripts := recorderOf(app).Scripts()
	if last := scripts[len(scripts)-1]; !strings.Contains(last, "systemctl --user status depper-tunnel@api.service --no-pager") || !strings.Contains(last, "DBUS_SESSION_BUS_ADDRESS") || stdout.String() != "unit output\n" {
		t.Errorf("script %q stdout %q", last, stdout.String())
	}

	app, stdout, _ = testApp(nil)
	recorderOf(app).Handler = fakeRemote("depper", func(c transporttest.Call) (transport.Result, error) {
		return transport.Result{Stdout: "journal\n"}, nil
	})
	if code := Run(context.Background(), app, []string{"tunnel-logs", repo, "--lines", "7"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	scripts = recorderOf(app).Scripts()
	if last := scripts[len(scripts)-1]; !strings.Contains(last, "journalctl --user-unit depper-tunnel@api.service --no-pager -n 7") || stdout.String() != "journal\n" {
		t.Errorf("script %q stdout %q", last, stdout.String())
	}

	app, _, stderr = testApp(nil)
	if code := Run(context.Background(), app, []string{"tunnel-logs", repo, "--lines", "0"}); code != exitcode.Failure || !strings.Contains(stderr.String(), "positive") || len(recorderOf(app).Calls()) != 0 {
		t.Errorf("--lines 0: exit %d %q", code, stderr.String())
	}
}

func TestTunnelStop(t *testing.T) {
	repo := repoWith(t, cliTunnelToml, nil)
	app, stdout, stderr := testApp(nil)
	recorderOf(app).Handler = tunnelRemote(cliTunnelState)
	if code := Run(context.Background(), app, []string{"tunnel-stop", repo}); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	scripts := recorderOf(app).Scripts()
	if last := scripts[len(scripts)-1]; !strings.Contains(last, "systemctl --user disable --now depper-tunnel@api.service") || !strings.Contains(stdout.String(), "DNS route are kept") {
		t.Errorf("script %q stdout %q", last, stdout.String())
	}

	app, _, stderr = testApp(nil)
	recorderOf(app).Handler = tunnelRemote("")
	if code := Run(context.Background(), app, []string{"tunnel-stop", repo}); code != exitcode.Failure || !strings.Contains(stderr.String(), "No owned tunnel is configured for this service") {
		t.Errorf("exit %d stderr %q", code, stderr.String())
	}
	if strings.Contains(strings.Join(recorderOf(app).Scripts(), "\n"), "disable") {
		t.Error("stop ran without owned state")
	}
}
