package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/frgmt0/depper/internal/shell"
)

// StaleLockAge is how old a lock must be before `depper unlock` removes it
// without --force.
const StaleLockAge = 15 * time.Minute

// LockInfo is the content of <base>/.depper/lock/info.
type LockInfo struct {
	CreatedAt string `json:"created_at"`
	Holder    string `json:"holder"`
	PID       int    `json:"pid"`
	Command   string `json:"command"`
}

func (i *LockInfo) describe() string {
	if i == nil {
		return "no lock info recorded"
	}
	return fmt.Sprintf("`depper %s` by %s (pid %d) since %s", i.Command, i.Holder, i.PID, i.CreatedAt)
}

// LockHeldError reports that another operation holds the lock.
type LockHeldError struct {
	Info *LockInfo
}

func (e *LockHeldError) Error() string {
	return "Another depper operation is already running for this service (" + e.Info.describe() + "); if it is stale, run `depper unlock`"
}

func parseLockInfo(raw string) *LockInfo {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var info LockInfo
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		return nil
	}
	info.Holder = sanitizeLabel(info.Holder)
	info.Command = sanitizeLabel(info.Command)
	info.CreatedAt = sanitizeLabel(info.CreatedAt)
	return &info
}

func (e *Engine) lockInfo() (string, error) {
	command := e.Command
	if command == "" {
		command = "deploy"
	}
	data, err := json.Marshal(LockInfo{CreatedAt: e.now().Format(time.RFC3339), Holder: e.Holder, PID: e.PID, Command: sanitizeLabel(command)})
	return string(data), err
}

// Lock acquires the deployment lock (mkdir is atomic) and records who holds
// it in <lock>/info.
func (e *Engine) Lock(ctx context.Context, l *Layout) error { return e.lockAt(ctx, l.Lock()) }

func (e *Engine) lockAt(ctx context.Context, lockPath string) error {
	info, err := e.lockInfo()
	if err != nil {
		return err
	}
	script := shell.Script(
		"set +xv",
		"set -eu",
		"umask 077",
		"lock="+shell.Quote(lockPath),
		`if mkdir "$lock" 2>/dev/null; then`,
		`  chmod 700 "$lock"`,
		`  printf '%s\n' `+shell.Quote(info)+` > "$lock/info"`,
		`  echo acquired`,
		`elif test -d "$lock" && ! test -L "$lock"; then`,
		`  echo held`,
		`  if test -f "$lock/info" && ! test -L "$lock/info"; then head -c 4096 "$lock/info"; fi`,
		`else`,
		`  echo "Cannot create the deployment lock" >&2; exit 1`,
		`fi`,
	)
	out, err := e.Remote.Capture(ctx, script)
	if err != nil {
		return err
	}
	status, rest, _ := strings.Cut(out, "\n")
	switch strings.TrimSpace(status) {
	case "acquired":
		return nil
	case "held":
		return &LockHeldError{Info: parseLockInfo(rest)}
	}
	return fmt.Errorf("remote host returned an invalid lock response")
}

// Unlock releases a lock acquired by Lock.
func (e *Engine) Unlock(ctx context.Context, l *Layout) error {
	return e.Remote.Run(ctx, shell.Script(
		"set +xv",
		"set -eu",
		"lock="+shell.Quote(l.Lock()),
		`rm -f "$lock/info"`,
		`rmdir "$lock"`,
	))
}

// WithLock runs fn while holding the deployment lock. The lock is released
// even when ctx is cancelled.
func (e *Engine) WithLock(ctx context.Context, l *Layout, fn func() error) error {
	if err := e.Lock(ctx, l); err != nil {
		return err
	}
	err := fn()
	if uerr := e.Unlock(withoutCancel(ctx), l); uerr != nil {
		return joinErrors(err, "releasing the deployment lock failed", uerr)
	}
	return err
}

// LockStatus describes the lock for `unlock` and `doctor`.
type LockStatus struct {
	Held bool      `json:"held"`
	Info *LockInfo `json:"info"`
	// Age is the lock's age; from info.created_at, else the directory mtime
	// (OlderThanStale only).
	Age time.Duration `json:"-"`
	// AgeSeconds is Age in whole seconds (-1 when unknown).
	AgeSeconds int64 `json:"age_seconds"`
	// Stale reports an age of at least StaleLockAge.
	Stale bool `json:"stale"`
}

// ReadLock inspects the lock without changing it.
func (e *Engine) ReadLock(ctx context.Context, l *Layout) (*LockStatus, error) {
	script := shell.Script(
		"set +xv",
		"set -eu",
		"lock="+shell.Quote(l.Lock()),
		`if ! test -d "$lock"; then echo free; exit 0; fi`,
		`echo held`,
		// -mmin is supported by GNU and BSD find; used when info is absent.
		`if test -n "$(find "$lock" -prune -mmin +`+fmt.Sprint(int(StaleLockAge/time.Minute))+` 2>/dev/null)"; then echo old; else echo young; fi`,
		`if test -f "$lock/info" && ! test -L "$lock/info"; then head -c 4096 "$lock/info"; fi`,
	)
	out, err := e.Remote.Capture(ctx, script)
	if err != nil {
		return nil, err
	}
	lines := strings.SplitN(out, "\n", 3)
	if strings.TrimSpace(lines[0]) == "free" {
		return &LockStatus{AgeSeconds: -1}, nil
	}
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != "held" {
		return nil, fmt.Errorf("remote host returned an invalid lock response")
	}
	status := &LockStatus{Held: true, AgeSeconds: -1}
	dirOld := strings.TrimSpace(lines[1]) == "old"
	if len(lines) == 3 {
		status.Info = parseLockInfo(lines[2])
	}
	if status.Info != nil {
		if created, err := time.Parse(time.RFC3339, status.Info.CreatedAt); err == nil {
			status.Age = e.now().Sub(created)
			status.AgeSeconds = int64(status.Age / time.Second)
			status.Stale = status.Age >= StaleLockAge
			return status, nil
		}
	}
	status.Stale = dirOld
	return status, nil
}

// ForceUnlockResult reports what `depper unlock` did.
type ForceUnlockResult struct {
	Removed bool        `json:"removed"`
	Lock    *LockStatus `json:"lock"`
}

// BreakLock removes the lock. Without force it refuses a lock younger than
// StaleLockAge.
func (e *Engine) BreakLock(ctx context.Context, force bool) (*ForceUnlockResult, error) {
	l, err := e.Probe(ctx, false)
	if err != nil {
		return nil, err
	}
	if !l.Deployed() {
		return &ForceUnlockResult{Lock: &LockStatus{AgeSeconds: -1}}, nil
	}
	status, err := e.ReadLock(ctx, l)
	if err != nil {
		return nil, err
	}
	if !status.Held {
		return &ForceUnlockResult{Lock: status}, nil
	}
	if !status.Stale && !force {
		return nil, fmt.Errorf("the lock is held by %s and is younger than %d minutes; wait for that operation or use --force", status.Info.describe(), int(StaleLockAge/time.Minute))
	}
	if err := e.Unlock(ctx, l); err != nil {
		return nil, err
	}
	return &ForceUnlockResult{Removed: true, Lock: status}, nil
}
