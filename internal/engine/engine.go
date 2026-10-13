// Package engine implements the depper deployment lifecycle on a remote
// host: the base-path probe, the deployment lock, private atomic uploads,
// the deploy transaction (release, environment, unit, setup, activation,
// health wait, automatic restore), rollback, pruning, status, health,
// secrets fingerprints, and the migration of Python deployer layouts.
//
// Every remote action is a POSIX sh script sent on ssh's stdin through
// transport.Remote; every interpolated value is validated and quoted with
// shell.Quote. Secret values never appear in argv, rsync, or output.
package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"regexp"
	"strings"
	"time"

	"github.com/frgmt0/depper/internal/config"
	"github.com/frgmt0/depper/internal/shell"
	"github.com/frgmt0/depper/internal/transport"
)

// DefaultSettle is how long a deploy waits after restarting the unit before
// checking it when no [health] check is configured (the Python deployer's
// fixed `sleep 3`).
const DefaultSettle = 3 * time.Second

// Engine runs lifecycle operations for one resolved configuration.
type Engine struct {
	Config *config.Config
	// Remote reaches the deploy host.
	Remote *transport.Remote
	// Local runs local programs (git).
	Local transport.Executor
	// Out receives human-readable progress and summaries.
	Out io.Writer
	// Command names the running depper command; it is recorded in the lock
	// info file.
	Command string

	// Now, Sleep, and Rand are injectable for tests.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
	Rand  io.Reader
	// Settle is the post-restart wait used when no [health] is configured.
	Settle time.Duration
	// Holder identifies this machine in lock info ("user@hostname").
	Holder string
	// PID is recorded in lock info.
	PID int

	// LockRefresh is how often a held lock is refreshed (touched) while an
	// operation runs; 0 disables refreshing.
	LockRefresh time.Duration

	// lastStamp is the last id timestamp issued (see newStamp).
	lastStamp time.Time

	// held is the deployment lock this engine holds.
	held *heldLock

	// forwardingProhibited caches that the host refused `ssh -W`, so later
	// url checks go straight to remote curl.
	forwardingProhibited bool
}

// New returns an Engine for cfg.
func New(cfg *config.Config, remote *transport.Remote, local transport.Executor, out io.Writer) *Engine {
	if out == nil {
		out = io.Discard
	}
	return &Engine{
		Config:      cfg,
		Remote:      remote,
		Local:       local,
		Out:         out,
		Now:         time.Now,
		Sleep:       sleepContext,
		Rand:        rand.Reader,
		Settle:      DefaultSettle,
		LockRefresh: DefaultLockRefresh,
		Holder:      localHolder(),
		PID:         os.Getpid(),
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// localHolder returns "user@hostname" for lock info and release metadata.
func localHolder() string {
	name := "unknown"
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return sanitizeLabel(name + "@" + host)
}

// sanitizeLabel removes control characters and bounds the length of an
// informational string.
func sanitizeLabel(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

func (e *Engine) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(e.Out, format, args...)
}

func (e *Engine) now() time.Time { return e.Now().UTC() }

func (e *Engine) randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(e.Rand, buf); err != nil {
		return "", fmt.Errorf("generating random data: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// releaseIDRe matches release identifiers: release-YYYYMMDDTHHMMSS.mmmZ-<hex>
// (depper), and the second-resolution form without .mmm (older depper
// versions and the Python deployer).
var releaseIDRe = regexp.MustCompile(`^release-[0-9]{8}T[0-9]{6}(\.[0-9]{3})?Z-[0-9a-f]{8}$`)

// ValidReleaseID reports whether id is a well-formed release identifier.
func ValidReleaseID(id string) bool { return releaseIDRe.MatchString(id) }

// idStampLayouts parse the timestamp part of release and transaction ids.
var idStampLayouts = []string{"20060102T150405.000Z", "20060102T150405Z"}

// idTime returns the time encoded in a <kind>-<stamp>-<hex> id (zero when
// it has none).
func idTime(id string) time.Time {
	_, rest, ok := strings.Cut(id, "-")
	if !ok {
		return time.Time{}
	}
	stamp, _, _ := strings.Cut(rest, "-")
	for _, layout := range idStampLayouts {
		if t, err := time.Parse(layout, stamp); err == nil {
			return t
		}
	}
	return time.Time{}
}

// idNewer orders ids newest first: by their encoded time (millisecond
// resolution; second resolution for older ids), then by the id.
func idNewer(a, b string) bool {
	ta, tb := idTime(a), idTime(b)
	if !ta.Equal(tb) {
		return ta.After(tb)
	}
	return a > b
}

// newStamp returns the id timestamp (millisecond resolution), strictly after
// any stamp this engine issued before, so ids made in quick succession
// still sort in creation order.
func (e *Engine) newStamp() string {
	t := e.now().Truncate(time.Millisecond)
	if !t.After(e.lastStamp) {
		t = e.lastStamp.Add(time.Millisecond)
	}
	e.lastStamp = t
	return t.Format("20060102T150405.000Z")
}

// newReleaseID returns release-YYYYMMDDTHHMMSS.mmmZ-<8 hex>.
func (e *Engine) newReleaseID() (string, error) {
	suffix, err := e.randomHex(4)
	if err != nil {
		return "", err
	}
	return "release-" + e.newStamp() + "-" + suffix, nil
}

// newTransactionID returns <kind>-YYYYMMDDTHHMMSS.mmmZ-<8 hex> for
// transactions that are not tied to a release (secrets push, rollback).
func (e *Engine) newTransactionID(kind string) (string, error) {
	suffix, err := e.randomHex(4)
	if err != nil {
		return "", err
	}
	return kind + "-" + e.newStamp() + "-" + suffix, nil
}

// unitPathExpr is the shell expression for the app unit file path.
func unitPathExpr(unitName string) string {
	return `"$HOME/.config/systemd/user/"` + shell.Quote(unitName)
}

// section is one "@@name" block of a script's output.
type section struct {
	Name string
	Body string
}

// parseSections splits script output into blocks introduced by lines that
// start with "@@". Text before the first marker is ignored. A section named
// rawLast (if non-empty) consumes the rest of the output verbatim.
func parseSections(out, rawLast string) []section {
	var result []section
	lines := strings.SplitAfter(out, "\n")
	var cur *section
	var body strings.Builder
	flush := func() {
		if cur != nil {
			cur.Body = body.String()
			result = append(result, *cur)
		}
		body.Reset()
	}
	for i, line := range lines {
		trimmed := strings.TrimSuffix(line, "\n")
		if strings.HasPrefix(trimmed, "@@") {
			flush()
			cur = &section{Name: strings.TrimPrefix(trimmed, "@@")}
			if rawLast != "" && cur.Name == rawLast {
				cur.Body = strings.Join(lines[i+1:], "")
				result = append(result, *cur)
				return result
			}
			continue
		}
		if cur != nil {
			body.WriteString(line)
		}
	}
	flush()
	return result
}

// sectionMap returns the sections keyed by name (the last one wins), with
// the trailing newline of each body removed.
func sectionMap(out string) map[string]string {
	m := map[string]string{}
	for _, s := range parseSections(out, "") {
		m[s.Name] = strings.TrimSuffix(s.Body, "\n")
	}
	return m
}

// refuseSymlink returns a script line that fails when path (a shell word)
// is a symbolic link.
func refuseSymlink(path, what string) string {
	return "test ! -L " + path + " || { echo " + shell.Quote("Refusing a symlink "+what) + " >&2; exit 1; }"
}

// withoutCancel returns a context for cleanup steps that must run even when
// the operation was interrupted.
func withoutCancel(ctx context.Context) context.Context { return context.WithoutCancel(ctx) }

// joinErrors formats a primary failure and a follow-up failure.
func joinErrors(primary error, what string, secondary error) error {
	if secondary == nil {
		return primary
	}
	if primary == nil {
		return fmt.Errorf("%s: %w", what, secondary)
	}
	return fmt.Errorf("%w; %s: %v", primary, what, secondary)
}

// ErrNotDeployed is returned by operations that need an existing deployment.
var ErrNotDeployed = errors.New("this service has not been deployed yet; run `depper deploy` first")
