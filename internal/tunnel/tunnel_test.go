package tunnel

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frgmt0/depper/internal/engine"
)

const (
	statePath  = baseRel + "/.depper/tunnel.json"
	configPath = baseRel + "/.depper/cloudflared.yml"
	unitPath   = ".config/systemd/user/depper-tunnel@api.service"
	unitName   = "depper-tunnel@api.service"
)

func probe(t *testing.T, e *engine.Engine, create bool) *engine.Layout {
	t.Helper()
	l, err := e.Probe(context.Background(), create)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestRegisteredWithTheEngine(t *testing.T) {
	if engine.RegisteredTunnel() != engine.TunnelHooks(Default) {
		t.Fatal("the tunnel package did not register itself with the engine")
	}
}

func TestFirstRunCreatesRecordsRoutesAndStartsTheTunnel(t *testing.T) {
	h := newFakeHost(t)
	e, s := h.Engine(tunnelToml)
	m, d := newManager()
	state, err := m.Run(context.Background(), e)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, h.Out)
	}
	if *state != (State{Name: "depper-api", ID: tunnelID, Hostname: "api.demo.example.com", Service: "http://127.0.0.1:8080"}) {
		t.Errorf("state = %+v", state)
	}
	if len(d.lookups) != 1 || d.lookups[0] != "api.demo.example.com" {
		t.Errorf("DNS preflight lookups = %v", d.lookups)
	}
	got, err := ParseState(h.readHome(statePath))
	if err != nil || *got != *state {
		t.Errorf("recorded state = %+v (%v)", got, err)
	}
	if info, err := os.Stat(filepath.Join(h.Home, statePath)); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("state file mode: %v %v", info, err)
	}
	if yml := h.readHome(configPath); yml != engine.CloudflaredConfig(tunnelID, h.Home, "api.demo.example.com", "http://127.0.0.1:8080") || strings.Contains(yml, "$HOME") {
		t.Errorf("cloudflared.yml:\n%s", yml)
	}
	wantUnit := engine.TunnelUnitFile("api", filepath.Join(h.Root, "remote-bin/cloudflared"), filepath.Join(h.Home, configPath), tunnelID)
	if unit := h.readHome(unitPath); unit != wantUnit {
		t.Errorf("unit:\n%s\nwant:\n%s", unit, wantUnit)
	}
	if !h.exists("systemd/"+unitName+".enabled") || !h.exists("systemd/"+unitName+".active") {
		t.Error("connector unit not enabled and started")
	}
	if h.existsHome(baseRel+"/.depper/tunnel-unit.next") || h.existsHome(baseRel+"/.depper/lock") {
		t.Error("candidate unit or lock left behind")
	}
	if routes := h.log("cf-routes"); routes != "--overwrite-dns=false "+tunnelID+" api.demo.example.com\n" {
		t.Errorf("routes = %q", routes)
	}
	cflog := h.log("cloudflared.log")
	for _, want := range []string{"cloudflared tunnel list --output json", "cloudflared tunnel create depper-api", "tunnel ingress validate"} {
		if !strings.Contains(cflog, want) {
			t.Errorf("cloudflared log lacks %q:\n%s", want, cflog)
		}
	}
	if strings.Contains(cflog, "force=true") {
		t.Errorf("an inherited TUNNEL_FORCE_PROVISIONING_DNS reached cloudflared:\n%s", cflog)
	}
	if strings.Contains(h.Out.String(), "CREDENTIAL-SECRET-VALUE") {
		t.Errorf("tunnel create output was relayed:\n%s", h.Out)
	}
	if !strings.Contains(h.Out.String(), "Created Cloudflare Tunnel depper-api ("+tunnelID+")") {
		t.Errorf("output:\n%s", h.Out)
	}
	if len(s.d) != 1 || s.d[0] != DefaultConnectSettle {
		t.Errorf("sleeps = %v", s.d)
	}
	var lockScripts int
	for _, script := range h.Scripts() {
		if strings.Contains(script, `if mkdir "$lock"`) {
			lockScripts++
		}
	}
	if lockScripts != 1 {
		t.Errorf("tunnel did not take the deployment lock exactly once (%d)", lockScripts)
	}
}

func TestRedeployReusesTheOwnedTunnel(t *testing.T) {
	h := newFakeHost(t)
	e, _ := h.Engine(tunnelToml)
	m, d := newManager()
	ctx := context.Background()
	if _, err := m.Run(ctx, e); err != nil {
		t.Fatalf("first run: %v\n%s", err, h.Out)
	}
	if err := os.Remove(filepath.Join(h.Root, "systemd", unitName+".active")); err != nil {
		t.Fatal(err)
	}
	// Once routed, the hostname resolves; the owned state skips the preflight.
	d.resolves["api.demo.example.com"] = true
	h.writeFile("cf-next-id", "11111111-2222-4333-8444-555555555555\n")
	state, err := m.Run(ctx, e)
	if err != nil {
		t.Fatalf("second run: %v\n%s", err, h.Out)
	}
	if state.ID != tunnelID || len(d.lookups) != 1 {
		t.Errorf("state %+v, lookups %v", state, d.lookups)
	}
	if n := strings.Count(h.log("cloudflared.log"), "tunnel create"); n != 1 {
		t.Errorf("tunnel create ran %d times", n)
	}
	if n := strings.Count(h.log("cf-routes"), tunnelID); n != 2 {
		t.Errorf("route dns ran %d times, want once per run", n)
	}
	if !h.exists("systemd/" + unitName + ".active") {
		t.Error("connector not restarted")
	}
}

func TestMigratedLegacyStateIsAccepted(t *testing.T) {
	h := newFakeHost(t)
	h.writeHome(statePath, ownedState("deployer-api"))
	h.writeFile("cf-tunnels", tunnelID+" deployer-api\n")
	e, _ := h.Engine(tunnelToml)
	m, d := newManager()
	d.resolves["api.demo.example.com"] = true
	state, err := m.Run(context.Background(), e)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, h.Out)
	}
	if state.Name != "deployer-api" || state.ID != tunnelID || len(d.lookups) != 0 || strings.Contains(h.log("cloudflared.log"), "tunnel create") {
		t.Errorf("state %+v lookups %v log:\n%s", state, d.lookups, h.log("cloudflared.log"))
	}
	if !strings.Contains(h.readHome(statePath), `"name": "deployer-api"`) {
		t.Error("migrated state was rewritten")
	}
	if !strings.Contains(h.readHome(unitPath), engine.TunnelUnitMarker("api")) {
		t.Error("connector unit not installed")
	}
}

// assertUntouched checks that nothing on the Cloudflare side or in the
// deployment's tunnel files changed.
func assertUntouched(t *testing.T, h *fakeHost) {
	t.Helper()
	if log := h.log("cloudflared.log"); strings.Contains(log, "tunnel create") || strings.Contains(log, "route dns") || strings.Contains(log, "ingress") {
		t.Errorf("cloudflared modified state:\n%s", log)
	}
	if h.existsHome(configPath) || h.existsHome(unitPath) || h.exists("cf-routes") {
		t.Error("tunnel config, unit, or route written")
	}
}

func TestRefusesHostnameOrOriginDrift(t *testing.T) {
	for name, toml := range map[string]string{
		"hostname": strings.Replace(tunnelToml, "api.demo.example.com", "other.demo.example.com", 1),
		"service":  strings.Replace(tunnelToml, "127.0.0.1:8080", "127.0.0.1:9090", 1),
	} {
		t.Run(name, func(t *testing.T) {
			h := newFakeHost(t)
			h.writeHome(statePath, ownedState("depper-api"))
			e, _ := h.Engine(toml)
			m, _ := newManager()
			if _, err := m.Run(context.Background(), e); !errors.Is(err, ErrDrift) {
				t.Fatalf("err = %v", err)
			}
			assertUntouched(t, h)
			if h.existsHome(baseRel + "/.depper/lock") {
				t.Error("lock left behind")
			}
		})
	}
}

func TestRefusesInvalidOrForeignState(t *testing.T) {
	cases := map[string]struct {
		state string
		want  error
	}{
		"bad id":        {strings.Replace(ownedState("depper-api"), tunnelID, "not-a-uuid", 1), ErrInvalidStateID},
		"extra field":   {`{"name":"depper-api","id":"` + tunnelID + `","hostname":"api.demo.example.com","service":"http://127.0.0.1:8080","extra":"x"}`, ErrInvalidState},
		"missing field": {`{"name":"depper-api","id":"` + tunnelID + `","hostname":"api.demo.example.com"}`, ErrInvalidState},
		"not json":      {"{oops", ErrStateNotJSON},
		"array":         {"[]", ErrInvalidState},
		"other service": {ownedState("depper-web"), ErrForeignState},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newFakeHost(t)
			h.writeHome(statePath, tc.state)
			e, _ := h.Engine(tunnelToml)
			m, d := newManager()
			if _, err := m.Run(context.Background(), e); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			assertUntouched(t, h)
			if len(d.lookups) != 0 || h.readHome(statePath) != tc.state {
				t.Error("state replaced or preflight ran")
			}
		})
	}
}

func TestRefusesToAdoptAnExistingTunnel(t *testing.T) {
	h := newFakeHost(t)
	h.writeFile("cf-tunnels", tunnelID+" depper-api\n")
	e, _ := h.Engine(tunnelToml)
	m, _ := newManager()
	_, err := m.Run(context.Background(), e)
	if err == nil || !strings.Contains(err.Error(), `Tunnel "depper-api" already exists but is not owned by this deployment`) {
		t.Fatalf("err = %v", err)
	}
	assertUntouched(t, h)
	if h.existsHome(statePath) {
		t.Error("foreign tunnel adopted")
	}
}

func TestDNSPreflightRefusesAResolvingHostname(t *testing.T) {
	h := newFakeHost(t)
	e, _ := h.Engine(tunnelToml)
	m, d := newManager()
	d.resolves["api.demo.example.com"] = true
	_, err := m.Run(context.Background(), e)
	if err == nil || !strings.Contains(err.Error(), "api.demo.example.com already resolves; refusing to risk a DNS conflict") {
		t.Fatalf("err = %v", err)
	}
	if h.log("cloudflared.log") != "" || h.existsHome(statePath) {
		t.Errorf("cloudflared ran before the preflight:\n%s", h.log("cloudflared.log"))
	}

	// A lookup failure other than NXDOMAIN fails closed too.
	d.resolves = map[string]bool{}
	d.err = errors.New("i/o timeout")
	if _, err := m.Run(context.Background(), e); err == nil || !strings.Contains(err.Error(), "DNS lookup failed: i/o timeout") {
		t.Fatalf("err = %v", err)
	}
	if h.log("cloudflared.log") != "" {
		t.Error("cloudflared ran after a failed lookup")
	}
}

func TestUnidentifiedCreatedTunnelIsNotAdopted(t *testing.T) {
	h := newFakeHost(t)
	h.writeFile("cf-create-unlisted", "")
	e, _ := h.Engine(tunnelToml)
	m, _ := newManager()
	if _, err := m.Run(context.Background(), e); err == nil || !strings.Contains(err.Error(), "could not be identified") {
		t.Fatalf("err = %v", err)
	}
	if h.existsHome(statePath) || h.existsHome(configPath) {
		t.Error("state fabricated for an unidentified tunnel")
	}
}

func TestRefusesToReplaceAnUnownedUnit(t *testing.T) {
	h := newFakeHost(t)
	h.writeHome(unitPath, "[Unit]\nDescription=someone else's connector\n")
	e, _ := h.Engine(tunnelToml)
	m, _ := newManager()
	if _, err := m.Run(context.Background(), e); err == nil || !strings.Contains(err.Error(), "Refusing to overwrite an unowned tunnel unit") {
		t.Fatalf("err = %v", err)
	}
	if h.readHome(unitPath) != "[Unit]\nDescription=someone else's connector\n" || h.existsHome(baseRel+"/.depper/tunnel-unit.next") {
		t.Error("unowned unit replaced or candidate left behind")
	}
}

func TestFailsWhenTheConnectorNeverConnects(t *testing.T) {
	h := newFakeHost(t)
	h.writeFile("cf-no-connect", "")
	e, s := h.Engine(tunnelToml)
	m, _ := newManager()
	if _, err := m.Run(context.Background(), e); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("err = %v", err)
	}
	if len(s.d) != DefaultConnectAttempts || s.d[0] != DefaultConnectSettle || s.d[1] != DefaultConnectInterval {
		t.Errorf("sleeps = %v", s.d)
	}
	if n := strings.Count(h.log("cloudflared.log"), "tunnel list"); n != 1+1+DefaultConnectAttempts {
		t.Errorf("tunnel list ran %d times", n)
	}
}

func TestFailsWhenTheConnectorUnitIsNotActive(t *testing.T) {
	h := newFakeHost(t)
	h.writeFile("systemd/"+unitName+".crash", "")
	e, _ := h.Engine(tunnelToml)
	m, _ := newManager()
	if _, err := m.Run(context.Background(), e); err == nil || !strings.Contains(err.Error(), unitName+" is not active after restart") {
		t.Fatalf("err = %v", err)
	}
}

func TestRequiresCloudflaredAndATunnelSection(t *testing.T) {
	h := newFakeHost(t)
	if err := os.Remove(filepath.Join(h.Root, "remote-bin/cloudflared")); err != nil {
		t.Fatal(err)
	}
	m, _ := newManager()
	e, _ := h.Engine(tunnelToml)
	if _, err := m.Run(context.Background(), e); err == nil || !strings.Contains(err.Error(), "cloudflared is not installed") {
		t.Fatalf("err = %v", err)
	}
	plain, _ := h.Engine(withoutTunnel(tunnelToml))
	if _, err := m.Run(context.Background(), plain); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v", err)
	}
}

func TestAssertNotOrphaned(t *testing.T) {
	h := newFakeHost(t)
	ctx := context.Background()
	plain, _ := h.Engine(withoutTunnel(tunnelToml))
	m, _ := newManager()
	l := probe(t, plain, true)
	if err := m.AssertNotOrphaned(ctx, plain, l); err != nil {
		t.Fatalf("no state: %v", err)
	}
	for _, name := range []string{"depper-api", "deployer-api"} {
		h.writeHome(statePath, ownedState(name))
		if err := m.AssertNotOrphaned(ctx, plain, probe(t, plain, false)); !errors.Is(err, ErrOrphaned) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	h.writeHome(statePath, "{oops")
	if err := m.AssertNotOrphaned(ctx, plain, probe(t, plain, false)); !errors.Is(err, ErrStateNotJSON) {
		t.Errorf("invalid state: err = %v", err)
	}
	// With [tunnel] configured it never refuses.
	withTunnel, _ := h.Engine(tunnelToml)
	if err := m.AssertNotOrphaned(ctx, withTunnel, probe(t, withTunnel, false)); err != nil {
		t.Errorf("with [tunnel]: %v", err)
	}
}

// useManager registers m with the engine for the test.
func useManager(t *testing.T, m *Manager) {
	engine.RegisterTunnel(m)
	t.Cleanup(func() { engine.RegisterTunnel(Default) })
}

func TestDeployEnsuresTheTunnelAndReportsItsFailure(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync not available")
	}
	h := newFakeHost(t)
	m, _ := newManager()
	useManager(t, m)
	ctx := context.Background()
	e, _ := h.Engine(tunnelToml)

	// The tunnel is created but routing fails: the app stays deployed and the
	// owned state is kept for a safe retry.
	h.writeFile("cf-route-fail", "")
	_, err := e.Deploy(ctx, engine.DeployOptions{})
	if err == nil || !strings.HasPrefix(err.Error(), "Application deployed successfully, but tunnel setup failed: ") || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v\n%s", err, h.Out)
	}
	if !h.existsHome(baseRel+"/current") || h.existsHome(baseRel+"/.depper/lock") || !h.existsHome(statePath) {
		t.Fatal("app not deployed, lock left behind, or owned state lost")
	}

	if err := os.Remove(filepath.Join(h.Root, "cf-route-fail")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Deploy(ctx, engine.DeployOptions{}); err != nil {
		t.Fatalf("retry: %v\n%s", err, h.Out)
	}
	if n := strings.Count(h.log("cloudflared.log"), "tunnel create"); n != 1 {
		t.Errorf("tunnel create ran %d times", n)
	}
	if !h.exists("systemd/" + unitName + ".active") {
		t.Error("connector not running after deploy")
	}

	// Removing [tunnel] while the state exists is refused by the hook.
	plain, _ := h.Engine(withoutTunnel(tunnelToml))
	if _, err := plain.Deploy(ctx, engine.DeployOptions{}); !errors.Is(err, ErrOrphaned) {
		t.Fatalf("orphan err = %v", err)
	}
}

func TestStatusLogsAndStop(t *testing.T) {
	h := newFakeHost(t)
	ctx := context.Background()
	e, _ := h.Engine(tunnelToml)
	probe(t, e, true)

	if err := Stop(ctx, e); !errors.Is(err, ErrNoOwnedTunnel) {
		t.Fatalf("stop without state: %v", err)
	}
	if strings.Contains(h.log("systemctl.log"), "disable") {
		t.Error("stop ran without owned state")
	}

	if err := Status(ctx, e); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.Out.String(), "fake status of "+unitName) || !strings.Contains(h.log("systemctl.log"), "systemctl --user status "+unitName+" --no-pager") {
		t.Errorf("status output %q log %q", h.Out, h.log("systemctl.log"))
	}

	var logs bytes.Buffer
	if err := Logs(ctx, e, 50, &logs); err != nil {
		t.Fatal(err)
	}
	if h.log("journalctl.log") != "journalctl --user-unit "+unitName+" --no-pager -n 50\n" || !strings.Contains(logs.String(), "journal line") {
		t.Errorf("journalctl log %q output %q", h.log("journalctl.log"), logs.String())
	}
	if err := Logs(ctx, e, 0, &logs); err == nil {
		t.Error("--lines 0 accepted")
	}

	h.writeHome(statePath, ownedState("depper-api"))
	h.writeFile("systemd/"+unitName+".active", "")
	h.writeFile("systemd/"+unitName+".enabled", "")
	if err := Stop(ctx, e); err != nil {
		t.Fatal(err)
	}
	if h.exists("systemd/"+unitName+".active") || h.exists("systemd/"+unitName+".enabled") || !h.existsHome(statePath) {
		t.Error("stop did not disable --now the connector, or removed the state")
	}
	for _, script := range h.Scripts() {
		if strings.Contains(script, "systemctl") && !strings.Contains(script, "DBUS_SESSION_BUS_ADDRESS") {
			t.Errorf("systemctl script without the user-bus prelude:\n%s", script)
		}
	}
}

func TestParseList(t *testing.T) {
	if got, err := parseList("null\n"); err != nil || got != nil {
		t.Errorf("null: %v %v", got, err)
	}
	for _, bad := range []string{"", "{}", "not json", `{"name":"x"}`} {
		if _, err := parseList(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	got, err := parseList(`[1, "x", {"name":"a","id":5}, {"name":"b","id":"` + tunnelID + `","connections":[{},{}]}, {"name":"c","id":"x","connections":null}]`)
	if err != nil || len(got) != 3 {
		t.Fatalf("rows = %+v (%v)", got, err)
	}
	if got[0].Name != "a" || got[0].HasID || got[1].ID != tunnelID || got[1].Connections != 2 || got[2].Connections != 0 {
		t.Errorf("rows = %+v", got)
	}
}

func TestParseState(t *testing.T) {
	if s, err := ParseState(" \n"); s != nil || err != nil {
		t.Errorf("blank: %v %v", s, err)
	}
	s, err := ParseState(ownedState("depper-api"))
	if err != nil || s.ID != tunnelID || s.Name != "depper-api" {
		t.Fatalf("%+v %v", s, err)
	}
	again, err := ParseState(string(s.encode()))
	if err != nil || *again != *s {
		t.Errorf("round trip: %+v %v", again, err)
	}
	if _, err := ParseState(`{"name":"depper-api","id":"` + tunnelID + `","hostname":"a\nb","service":"http://x:1"}`); !errors.Is(err, ErrInvalidState) {
		t.Errorf("control characters accepted: %v", err)
	}
	if _, err := ParseState(`{"name":"","id":"` + tunnelID + `","hostname":"a","service":"b"}`); !errors.Is(err, ErrInvalidState) {
		t.Errorf("empty name accepted: %v", err)
	}
}

// Text a login shell prints before the script runs (motd, rc files) must not
// break the state, cloudflared path, or tunnel list parsing.
func TestLoginShellNoiseIsIgnored(t *testing.T) {
	h := newFakeHost(t)
	h.writeFile("login-noise", "Welcome!\nstate=noise\n[{\"name\":\"x\"}]\n")
	e, _ := h.Engine(tunnelToml)
	m, _ := newManager()
	ctx := context.Background()
	state, err := m.Run(ctx, e)
	if err != nil {
		t.Fatalf("run with login noise: %v\n%s", err, h.Out)
	}
	if state.ID != tunnelID {
		t.Errorf("state = %+v", state)
	}
	if _, err := m.Run(ctx, e); err != nil {
		t.Fatalf("second run with login noise: %v\n%s", err, h.Out)
	}
}
