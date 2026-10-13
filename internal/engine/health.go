package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/frgmt0/depper/internal/config"
	"github.com/frgmt0/depper/internal/shell"
	"github.com/frgmt0/depper/internal/transport"
)

// BodyLimit is the maximum health response body depper keeps.
const BodyLimit = 4096

// Health check kinds and methods (HealthResult.Check / .Method).
const (
	CheckURL     = "url"
	CheckCommand = "command"
	CheckUnit    = "unit"

	MethodForward    = "ssh-forward"
	MethodCurl       = "remote-curl"
	MethodCommand    = "remote-command"
	MethodSystemd    = "systemd"
	healthErrorLimit = 512
)

// HealthResult is one health check. JSON keys are stable:
//
//	ok          bool    the check passed
//	check       string  "url", "command", or "unit" (no [health]: unit active)
//	method      string  "ssh-forward", "remote-curl", "remote-command", "systemd"
//	target      string  URL, command, or unit name
//	status      int     HTTP status (0 when not an HTTP check or no response)
//	latency_ms  int     duration of the check in milliseconds
//	body        any     response body or command output (<= 4 KiB): parsed
//	                    JSON when valid, else a string; null when empty
//	error       string  why the check failed ("" when ok)
//	checked_at  string  UTC RFC 3339 time of the check
type HealthResult struct {
	OK        bool            `json:"ok"`
	Check     string          `json:"check"`
	Method    string          `json:"method"`
	Target    string          `json:"target"`
	Status    int             `json:"status"`
	LatencyMS int64           `json:"latency_ms"`
	Body      json.RawMessage `json:"body"`
	Error     string          `json:"error"`
	CheckedAt string          `json:"checked_at"`
}

// Summary is a one-line human description.
func (r *HealthResult) Summary() string {
	if r == nil {
		return "not checked"
	}
	var b strings.Builder
	if r.OK {
		b.WriteString("ok")
	} else {
		b.WriteString("FAILED")
	}
	var details []string
	switch r.Check {
	case CheckURL:
		if r.Status != 0 {
			details = append(details, "HTTP "+strconv.Itoa(r.Status))
		}
		details = append(details, fmt.Sprintf("%d ms", r.LatencyMS), r.Method)
	case CheckCommand:
		details = append(details, "command", fmt.Sprintf("%d ms", r.LatencyMS))
	case CheckUnit:
		details = append(details, "unit active check; no [health] configured")
	}
	if r.Error != "" {
		details = append(details, r.Error)
	}
	if len(details) > 0 {
		b.WriteString(" (" + strings.Join(details, ", ") + ")")
	}
	return b.String()
}

// encodeBody returns body as JSON when it parses, else as a JSON string,
// and null when empty.
func encodeBody(body []byte) json.RawMessage {
	if len(body) > BodyLimit {
		body = body[:BodyLimit]
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return json.RawMessage("null")
	}
	if json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed)
	}
	data, _ := json.Marshal(strings.ToValidUTF8(string(body), "�"))
	return data
}

func limitError(msg string) string {
	msg = strings.TrimSpace(msg)
	if len(msg) > healthErrorLimit {
		msg = msg[:healthErrorLimit]
	}
	return sanitizeLabel(strings.ReplaceAll(msg, "\n", "; "))
}

func statusOK(h *config.Health, status int) bool {
	if h.ExpectStatus != 0 {
		return status == h.ExpectStatus
	}
	return status >= 200 && status <= 299
}

func (e *Engine) statusError(status int) string {
	if e.Config.Health.ExpectStatus != 0 {
		return fmt.Sprintf("expected HTTP %d, got %d", e.Config.Health.ExpectStatus, status)
	}
	return fmt.Sprintf("expected a 2xx HTTP status, got %d", status)
}

// CheckHealth runs the configured [health] check once; without [health] it
// reports whether the unit is active.
func (e *Engine) CheckHealth(ctx context.Context, l *Layout) (*HealthResult, error) {
	h := e.Config.Health
	switch {
	case h == nil:
		st, err := e.UnitState(ctx)
		if err != nil {
			return nil, err
		}
		r := &HealthResult{Check: CheckUnit, Method: MethodSystemd, Target: e.Config.UnitName(), Body: json.RawMessage("null"), CheckedAt: e.now().Format(time.RFC3339)}
		r.OK = st.ActiveState == "active"
		if !r.OK {
			r.Error = "unit is " + st.describe()
		}
		return r, nil
	case h.URL != "":
		return e.checkURL(ctx)
	default:
		return e.checkCommand(ctx, l)
	}
}

func (e *Engine) checkURL(ctx context.Context) (*HealthResult, error) {
	if !e.forwardingProhibited {
		r, err := e.checkURLForward(ctx)
		if err == nil {
			return r, nil
		}
		if !isForwardingProhibited(err) {
			return nil, err
		}
		e.forwardingProhibited = true
	}
	return e.checkURLCurl(ctx)
}

func isForwardingProhibited(err error) bool {
	return transport.IsForwardingProhibited(err) || strings.Contains(err.Error(), "administratively prohibited")
}

// minDialTimeout is the least time the SSH handshake of a forwarded url
// check gets (DialTimeout).
const minDialTimeout = 15 * time.Second

// DialTimeout bounds the SSH connection and authentication of a forwarded
// url check: max(15s, [health].timeout). [health].timeout itself bounds only
// the HTTP exchange (request, response headers, and body) after ssh is
// connected.
func DialTimeout(h *config.Health) time.Duration {
	if h.Timeout > minDialTimeout {
		return h.Timeout
	}
	return minDialTimeout
}

// checkURLForward requests the URL through `ssh -W`, from the host's network
// view. It returns an error only when forwarding itself is refused or ssh
// cannot connect or run; connection failures are an unhealthy result.
func (e *Engine) checkURLForward(ctx context.Context) (*HealthResult, error) {
	h := e.Config.Health
	r := &HealthResult{Check: CheckURL, Method: MethodForward, Target: h.URL, Body: json.RawMessage("null"), CheckedAt: e.now().Format(time.RFC3339)}
	dialTimeout := DialTimeout(h)
	var dialTimedOut atomic.Bool
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				dctx, cancel := context.WithTimeout(ctx, dialTimeout)
				defer cancel()
				conn, err := e.Remote.DialContext(dctx, network, addr)
				if err != nil {
					if dctx.Err() != nil && ctx.Err() == nil {
						dialTimedOut.Store(true)
						return nil, fmt.Errorf("ssh did not connect to the deploy host within %s", dialTimeout)
					}
					return nil, err
				}
				// From here on, [health].timeout bounds writing the request
				// and reading the response headers and body.
				if err := conn.SetDeadline(time.Now().Add(h.Timeout)); err != nil {
					conn.Close()
					return nil, err
				}
				return conn, nil
			},
			DisableKeepAlives: true,
			Proxy:             nil,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid [health].url: %w", err)
	}
	req.Header.Set("User-Agent", "depper-health")
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		var nf *transport.NotFoundError
		if isForwardingProhibited(err) || errors.As(err, &nf) || dialTimedOut.Load() {
			return nil, err
		}
		var cerr *transport.CommandError
		if errors.As(err, &cerr) && cerr.ExitCode == 255 && !strings.Contains(cerr.Stderr, "open failed") {
			return nil, err // ssh could not reach the host at all
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		r.LatencyMS = time.Since(start).Milliseconds()
		r.Error = limitError(unwrapURLError(err))
		if errors.Is(err, os.ErrDeadlineExceeded) {
			r.Error = "no complete response within " + seconds(h.Timeout) + "s"
		}
		return r, nil
	}
	defer resp.Body.Close()
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, BodyLimit))
	r.LatencyMS = time.Since(start).Milliseconds()
	r.Status = resp.StatusCode
	r.Body = encodeBody(body)
	if rerr != nil && errors.Is(rerr, os.ErrDeadlineExceeded) {
		r.Error = "the response body was not complete within " + seconds(h.Timeout) + "s"
		return r, nil
	}
	r.OK = statusOK(h, resp.StatusCode)
	if !r.OK {
		r.Error = e.statusError(resp.StatusCode)
	}
	return r, nil
}

func unwrapURLError(err error) string {
	var cerr *transport.CommandError
	if errors.As(err, &cerr) && strings.TrimSpace(cerr.Stderr) != "" {
		return cerr.Stderr
	}
	return err.Error()
}

// curlScript runs the URL check with curl on the host.
func curlScript(h *config.Health) string {
	timeout := seconds(h.Timeout)
	return shell.Script(
		"set +xv",
		"set -eu",
		`if ! command -v curl >/dev/null 2>&1; then echo @@missing; exit 0; fi`,
		`tmp=$(mktemp "${TMPDIR:-/tmp}/depper-health.XXXXXX")`,
		`trap 'rm -f "$tmp" "$tmp.err"' EXIT HUP INT TERM`,
		`rc=0`,
		`meta=$(curl -sS -o "$tmp" -w '%{http_code} %{time_total}' --max-time `+timeout+` `+shell.Quote(h.URL)+` 2>"$tmp.err") || rc=$?`,
		`printf '@@rc\n%s\n@@meta\n%s\n@@err\n' "$rc" "$meta"`,
		`head -c 512 "$tmp.err"`,
		`printf '\n@@body\n'`,
		`head -c 4096 "$tmp"`,
	)
}

func (e *Engine) checkURLCurl(ctx context.Context) (*HealthResult, error) {
	h := e.Config.Health
	r := &HealthResult{Check: CheckURL, Method: MethodCurl, Target: h.URL, Body: json.RawMessage("null"), CheckedAt: e.now().Format(time.RFC3339)}
	start := time.Now()
	out, err := e.Remote.Capture(ctx, curlScript(h))
	if err != nil {
		return nil, err
	}
	var rc, meta, stderr string
	body := ""
	for _, s := range parseSections(out, "body") {
		switch s.Name {
		case "missing":
			return nil, fmt.Errorf("the url health check cannot run: SSH TCP forwarding is disabled on the deploy host and curl is not installed there; enable AllowTcpForwarding for this user or install curl")
		case "rc":
			rc = strings.TrimSpace(s.Body)
		case "meta":
			meta = strings.TrimSpace(s.Body)
		case "err":
			stderr = s.Body
		case "body":
			body = s.Body
		}
	}
	fields := strings.Fields(meta)
	if rc == "" || len(fields) != 2 {
		return nil, fmt.Errorf("remote curl returned invalid output")
	}
	status, _ := strconv.Atoi(fields[0])
	if seconds, err := strconv.ParseFloat(fields[1], 64); err == nil && seconds > 0 {
		r.LatencyMS = int64(seconds * 1000)
	} else {
		r.LatencyMS = time.Since(start).Milliseconds()
	}
	r.Status = status
	r.Body = encodeBody([]byte(body))
	switch {
	case rc != "0":
		r.Error = limitError("curl failed: " + stderr)
		if strings.TrimSpace(stderr) == "" {
			r.Error = "curl exited with status " + rc
		}
	case statusOK(h, status):
		r.OK = true
	default:
		r.Error = e.statusError(status)
	}
	return r, nil
}

// healthOutputMarker precedes the health command's output, so anything a
// login shell printed before the script ran is not taken as the body.
const healthOutputMarker = "@@health-output"

// commandScript runs the health command in the current release with only
// [env] exported (never the secrets file). The command's exit status 255 is
// reported as 254, so 255 always means that ssh itself failed.
func (e *Engine) commandScript(l *Layout) string {
	command := shell.Quote(e.Config.Health.Command)
	lines := append([]string{"set +xv"}, shell.Prelude()...)
	lines = append(lines, shell.Exports(e.Config.Env)...)
	lines = append(lines,
		"cd "+shell.Quote(l.Current()),
		`echo `+healthOutputMarker,
		"rc=0",
		// coreutils timeout enforces [health].timeout on the host when present.
		"if command -v timeout >/dev/null 2>&1; then timeout "+seconds(e.Config.Health.Timeout)+" /bin/sh -lc "+command+" || rc=$?; else /bin/sh -lc "+command+" || rc=$?; fi",
		`if test "$rc" -eq 255; then exit 254; fi`,
		`exit "$rc"`,
	)
	return shell.Script(lines...)
}

func (e *Engine) checkCommand(ctx context.Context, l *Layout) (*HealthResult, error) {
	h := e.Config.Health
	r := &HealthResult{Check: CheckCommand, Method: MethodCommand, Target: h.Command, Body: json.RawMessage("null"), CheckedAt: e.now().Format(time.RFC3339)}
	cctx, cancel := context.WithTimeout(ctx, h.Timeout+15*time.Second)
	defer cancel()
	start := time.Now()
	out, err := e.Remote.Capture(cctx, e.commandScript(l))
	r.LatencyMS = time.Since(start).Milliseconds()
	r.Body = encodeBody([]byte(FromMarker(out, healthOutputMarker)))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var cerr *transport.CommandError
		var nf *transport.NotFoundError
		if errors.As(err, &nf) {
			return nil, err
		}
		if errors.As(err, &cerr) {
			switch cerr.ExitCode {
			case 255:
				// ssh itself failed (connection), not the command.
				return nil, err
			case 124:
				r.Error = "health command timed out after " + seconds(h.Timeout) + "s"
				return r, nil
			}
			msg := strings.TrimSpace(cerr.Stderr)
			if msg == "" {
				code := cerr.ExitCode
				if code == 254 {
					code = 255 // remapped by commandScript; may also be a real 254
					msg = fmt.Sprintf("health command exited with status %d (or 254)", code)
				} else {
					msg = fmt.Sprintf("health command exited with status %d", code)
				}
			}
			r.Error = limitError(msg)
			return r, nil
		}
		r.Error = limitError(err.Error())
		return r, nil
	}
	r.OK = true
	return r, nil
}

// UnitState is the systemd state of the app unit.
type UnitState struct {
	ActiveState          string `json:"active_state"`
	SubState             string `json:"sub_state"`
	MainPID              int    `json:"main_pid"`
	NRestarts            int    `json:"n_restarts"`
	ActiveEnterTimestamp string `json:"active_enter_timestamp"`
	// LoadState is "loaded", "not-found", ...
	LoadState string `json:"load_state"`
}

func (s *UnitState) describe() string {
	return s.ActiveState + "/" + s.SubState
}

func unitShowLines(unitName string) []string {
	return []string{
		"systemctl --user show -p LoadState -p ActiveState -p SubState -p MainPID -p NRestarts -p ActiveEnterTimestamp " + shell.Quote(unitName) + " 2>/dev/null || true",
	}
}

func parseUnitState(out string) *UnitState {
	s := &UnitState{}
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		value = sanitizeLabel(value)
		switch key {
		case "LoadState":
			s.LoadState = value
		case "ActiveState":
			s.ActiveState = value
		case "SubState":
			s.SubState = value
		case "MainPID":
			s.MainPID, _ = strconv.Atoi(value)
		case "NRestarts":
			s.NRestarts, _ = strconv.Atoi(value)
		case "ActiveEnterTimestamp":
			s.ActiveEnterTimestamp = value
		}
	}
	if s.ActiveState == "" {
		s.ActiveState = "unknown"
	}
	return s
}

// UnitState reads the app unit's systemd state.
func (e *Engine) UnitState(ctx context.Context) (*UnitState, error) {
	lines := append([]string{"set +xv"}, shell.Prelude()...)
	lines = append(lines, "echo @@unit-state")
	out, err := e.Remote.Capture(ctx, shell.Script(append(lines, unitShowLines(e.Config.UnitName())...)...))
	if err != nil {
		return nil, err
	}
	return parseUnitState(FromMarker(out, "@@unit-state")), nil
}

// WaitHealthy waits for the restarted unit to become healthy. Without
// [health] it waits Settle and requires the unit to be active with no
// restarts (the Python deployer's check). With [health] it polls every
// interval until start_period elapses, failing fast when the unit fails or
// restarts.
func (e *Engine) WaitHealthy(ctx context.Context, l *Layout) (*HealthResult, error) {
	h := e.Config.Health
	if h == nil {
		if err := e.Sleep(ctx, e.Settle); err != nil {
			return nil, err
		}
		st, err := e.UnitState(ctx)
		if err != nil {
			return nil, err
		}
		if st.ActiveState != "active" {
			return nil, fmt.Errorf("%s is not active after restart (%s)", e.Config.UnitName(), st.describe())
		}
		if st.NRestarts != 0 {
			return nil, fmt.Errorf("%s restarted %d times after start", e.Config.UnitName(), st.NRestarts)
		}
		return &HealthResult{OK: true, Check: CheckUnit, Method: MethodSystemd, Target: e.Config.UnitName(), Body: json.RawMessage("null"), CheckedAt: e.now().Format(time.RFC3339)}, nil
	}
	deadline := e.Now().Add(h.StartPeriod)
	var last *HealthResult
	for {
		st, err := e.UnitState(ctx)
		if err != nil {
			return nil, err
		}
		if st.ActiveState == "failed" || st.NRestarts > 0 {
			return last, fmt.Errorf("%s failed while waiting for health (%s, %d restarts)", e.Config.UnitName(), st.describe(), st.NRestarts)
		}
		if st.ActiveState == "active" {
			r, err := e.CheckHealth(ctx, l)
			if err != nil {
				return nil, err
			}
			if r.OK {
				return r, nil
			}
			last = r
		}
		remaining := deadline.Sub(e.Now())
		if remaining <= 0 {
			if last == nil {
				return nil, fmt.Errorf("%s did not become active within start_period (%s)", e.Config.UnitName(), st.describe())
			}
			return last, fmt.Errorf("health check did not pass within start_period: %s", last.Error)
		}
		wait := h.Interval
		if wait > remaining {
			wait = remaining
		}
		if err := e.Sleep(ctx, wait); err != nil {
			return last, err
		}
	}
}

// seconds formats d as a decimal number of seconds for timeout/curl.
func seconds(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', -1, 64)
}
