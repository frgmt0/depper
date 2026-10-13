package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/frgmt0/depper/internal/engine"
	"github.com/frgmt0/depper/internal/shell"
	"github.com/frgmt0/depper/internal/transport"
)

// cloudflaredCLI runs cloudflared on the deploy host at a validated
// absolute path, always with TUNNEL_FORCE_PROVISIONING_DNS cleared so an
// inherited setting cannot force a DNS overwrite.
type cloudflaredCLI struct {
	e    *engine.Engine
	path string
}

func (c *cloudflaredCLI) script(args ...string) string {
	return shell.Script(
		"set +xv",
		"set -eu",
		"unset TUNNEL_FORCE_PROVISIONING_DNS",
		shell.Quote(c.path)+" "+shell.Join(args...),
	)
}

// run runs the command and relays its stdout to the user (see runScript).
func (c *cloudflaredCLI) run(ctx context.Context, args ...string) error {
	return runScript(ctx, c.e, c.script(args...))
}

// runScript runs script with its output captured, relays stdout to the
// engine output, and on failure returns an error carrying the remote stderr
// (a streamed failure would only say that ssh exited, which is unhelpful in
// the deploy-time "tunnel setup failed" message).
func runScript(ctx context.Context, e *engine.Engine, script string) error {
	out, err := e.Remote.Capture(ctx, script)
	if out != "" {
		_, _ = io.WriteString(e.Out, out)
	}
	return err
}

// listMarker precedes `cloudflared tunnel list` JSON, so output a login
// shell printed first is not parsed as part of it.
const listMarker = "@@tunnel-list"

// list returns `cloudflared tunnel list --output json`, parsed locally.
func (c *cloudflaredCLI) list(ctx context.Context) ([]listedTunnel, error) {
	script := shell.Script(
		"set +xv",
		"set -eu",
		"unset TUNNEL_FORCE_PROVISIONING_DNS",
		"echo "+listMarker,
		shell.Quote(c.path)+" "+shell.Join("tunnel", "list", "--output", "json"),
	)
	out, err := c.e.Remote.Capture(ctx, script)
	if err != nil {
		return nil, err
	}
	return parseList(engine.FromMarker(out, listMarker))
}

// create runs `cloudflared tunnel create NAME`. Its stdout can carry
// credential details on some cloudflared releases, so it is captured and
// never relayed, not even in an error message.
func (c *cloudflaredCLI) create(ctx context.Context, name string) error {
	_, err := c.e.Remote.Capture(ctx, c.script("tunnel", "create", name))
	if err == nil {
		return nil
	}
	var cerr *transport.CommandError
	if errors.As(err, &cerr) {
		if msg := strings.TrimSpace(cerr.Stderr); msg != "" {
			return fmt.Errorf("cloudflared tunnel create failed: %s", msg)
		}
		return fmt.Errorf("cloudflared tunnel create failed with status %d", cerr.ExitCode)
	}
	return err
}

// listedTunnel is the part of one `tunnel list` JSON row depper uses.
type listedTunnel struct {
	Name        string
	ID          string
	HasID       bool // "id" is a JSON string
	Connections int  // number of active connections listed
}

var errInvalidList = errors.New("cloudflared returned invalid tunnel list data")

// parseList parses `cloudflared tunnel list --output json`: a JSON array of
// objects with at least "name" and "id" strings and a "connections" array.
// A JSON null (an empty list rendered from a nil slice) is an empty list.
// Rows of an unexpected shape are ignored rather than trusted.
func parseList(out string) ([]listedTunnel, error) {
	trimmed := strings.TrimSpace(out)
	if trimmed == "null" {
		return nil, nil
	}
	var rows []json.RawMessage
	if trimmed == "" || json.Unmarshal([]byte(trimmed), &rows) != nil {
		return nil, errInvalidList
	}
	var tunnels []listedTunnel
	for _, row := range rows {
		var fields map[string]json.RawMessage
		if json.Unmarshal(row, &fields) != nil || fields == nil {
			continue
		}
		var t listedTunnel
		if json.Unmarshal(fields["name"], &t.Name) != nil {
			t.Name = ""
		}
		if raw, ok := fields["id"]; ok && json.Unmarshal(raw, &t.ID) == nil {
			t.HasID = true
		}
		var conns []json.RawMessage
		if raw, ok := fields["connections"]; ok && json.Unmarshal(raw, &conns) == nil {
			t.Connections = len(conns)
		}
		tunnels = append(tunnels, t)
	}
	return tunnels, nil
}
