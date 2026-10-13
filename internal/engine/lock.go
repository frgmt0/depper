package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/frgmt0/depper/internal/shell"
)

// StaleLockAge is how long a lock must go without being refreshed before
// `depper unlock` removes it without --force. Running operations refresh
// their lock (touch the lock directory) every DefaultLockRefresh, so an
// active operation never looks stale. Ages are measured with the deploy
// host's clock.
const StaleLockAge = 15 * time.Minute

// DefaultLockRefresh is how often a held lock is refreshed.
const DefaultLockRefresh = StaleLockAge / 3

// remoteKeepaliveSeconds is how often scripts that run long remote commands
// (setup, container builds) refresh the lock from the host itself, so the
// lock stays fresh even if the client disappears while they run.
const remoteKeepaliveSeconds = 60

// LockInfo is the content of <base>/.depper/lock/info. The ownership token
// stored with it is not part of the JSON output.
type LockInfo struct {
	CreatedAt string `json:"created_at"`
	Holder    string `json:"holder"`
	PID       int    `json:"pid"`
	Command   string `json:"command"`

	token string
}

// lockFile is the on-disk form of LockInfo.
type lockFile struct {
	CreatedAt string `json:"created_at"`
	Holder    string `json:"holder"`
	PID       int    `json:"pid"`
	Command   string `json:"command"`
	Token     string `json:"token"`
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
	var f lockFile
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		return nil
	}
	info := &LockInfo{Holder: sanitizeLabel(f.Holder), Command: sanitizeLabel(f.Command), CreatedAt: sanitizeLabel(f.CreatedAt), PID: f.PID}
	if validToken(f.Token) {
		info.token = f.Token
	}
	return info
}

func validToken(t string) bool {
	if len(t) != 32 {
		return false
	}
	for i := 0; i < len(t); i++ {
		if !strings.ContainsRune("0123456789abcdef", rune(t[i])) {
			return false
		}
	}
	return true
}

// tokenPattern is the exact text of the token field in a lock info file
// (depper writes it with encoding/json, without spaces).
func tokenPattern(token string) string { return `"token":"` + token + `"` }

// heldLock is the lock this engine holds.
type heldLock struct {
	path  string
	token string
}

func (e *Engine) lockInfo(token string) (string, error) {
	command := e.Command
	if command == "" {
		command = "deploy"
	}
	data, err := json.Marshal(lockFile{CreatedAt: e.now().Format(time.RFC3339), Holder: e.Holder, PID: e.PID, Command: sanitizeLabel(command), Token: token})
	return string(data), err
}

// Lock acquires the deployment lock (mkdir is atomic) and records who holds
// it, with a random ownership token, in <lock>/info.
func (e *Engine) Lock(ctx context.Context, l *Layout) error { return e.lockAt(ctx, l.Lock()) }

const lockMarker = "@@lock"

func (e *Engine) lockAt(ctx context.Context, lockPath string) error {
	token, err := e.randomHex(16)
	if err != nil {
		return err
	}
	info, err := e.lockInfo(token)
	if err != nil {
		return err
	}
	script := shell.Script(
		"set +xv",
		"set -eu",
		"umask 077",
		"lock="+shell.Quote(lockPath),
		`echo `+lockMarker,
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
	body := FromMarker(out, lockMarker)
	status, rest, _ := strings.Cut(body, "\n")
	switch strings.TrimSpace(status) {
	case "acquired":
		e.held = &heldLock{path: lockPath, token: token}
		return nil
	case "held":
		return &LockHeldError{Info: parseLockInfo(rest)}
	}
	return fmt.Errorf("remote host returned an invalid lock response")
}

// releaseScript removes the lock at path only while its info still carries
// token (expectToken); with an empty token it removes a lock whose info has
// no token at all. Anything else fails loudly and leaves the lock alone.
func releaseScript(path, token string) string {
	check := `grep -Fq -e ` + shell.Quote(tokenPattern(token)) + ` "$lock/info"`
	if token == "" {
		check = `! grep -Fq -e '"token":' "$lock/info"`
	}
	return shell.Script(
		"set +xv",
		"set -eu",
		"lock="+shell.Quote(path),
		`if ! test -d "$lock" || test -L "$lock"; then echo "The deployment lock was removed by someone else (depper unlock?) while this operation ran" >&2; exit 1; fi`,
		`if test -f "$lock/info" && ! test -L "$lock/info"; then`,
		`  if ! `+check+`; then echo "The deployment lock is now held by someone else; it was not removed" >&2; exit 1; fi`,
		`elif test -n `+shell.Quote(token)+`; then`,
		`  echo "The deployment lock is now held by someone else; it was not removed" >&2; exit 1`,
		`fi`,
		`rm -f "$lock/info"`,
		`rmdir "$lock"`,
	)
}

// Unlock releases the lock acquired by Lock. It removes the lock only if it
// still carries this engine's token; otherwise it fails loudly ("the lock is
// now held by someone else") and leaves it alone.
func (e *Engine) Unlock(ctx context.Context, l *Layout) error {
	held := e.held
	if held == nil || held.path != l.Lock() {
		return fmt.Errorf("this operation does not hold the deployment lock")
	}
	if _, err := e.Remote.Capture(ctx, releaseScript(held.path, held.token)); err != nil {
		return err
	}
	e.held = nil
	return nil
}

// refreshScript touches the lock directory (its mtime is the lock's age)
// while the lock still carries token.
func refreshScript(path, token string) string {
	return shell.Script(
		"set +xv",
		"set -eu",
		"lock="+shell.Quote(path),
		`if test -d "$lock" && ! test -L "$lock" && grep -Fq -e `+shell.Quote(tokenPattern(token))+` "$lock/info" 2>/dev/null; then touch "$lock"; fi`,
	)
}

// lockKeepaliveLines start a background loop in a remote script that
// refreshes the held lock every remoteKeepaliveSeconds while the script's
// shell is alive and the lock is still ours; it is stopped when the script
// exits. Without a held lock they are empty.
func (e *Engine) lockKeepaliveLines() []string {
	held := e.held
	if held == nil {
		return nil
	}
	return []string{
		"depper_lock=" + shell.Quote(held.path),
		"depper_token=" + shell.Quote(tokenPattern(held.token)),
		`(`,
		`  while sleep ` + strconv.Itoa(remoteKeepaliveSeconds) + `; do`,
		`    kill -0 $$ 2>/dev/null || exit 0`,
		`    grep -Fq -e "$depper_token" "$depper_lock/info" 2>/dev/null || exit 0`,
		`    touch "$depper_lock" 2>/dev/null || true`,
		`  done`,
		`) </dev/null >/dev/null 2>&1 &`,
		`depper_keepalive=$!`,
		`trap 'kill "$depper_keepalive" 2>/dev/null || true' EXIT`,
	}
}

// WithLock runs fn while holding the deployment lock, refreshing it every
// LockRefresh. The lock is released even when ctx is cancelled, after fn
// (and so every remote command it started) has returned.
func (e *Engine) WithLock(ctx context.Context, l *Layout, fn func() error) error {
	if err := e.Lock(ctx, l); err != nil {
		return err
	}
	stop := e.startLockRefresh(ctx)
	err := fn()
	stop()
	if uerr := e.Unlock(withoutCancel(ctx), l); uerr != nil {
		return joinErrors(err, "releasing the deployment lock failed", uerr)
	}
	return err
}

// startLockRefresh refreshes the held lock every LockRefresh until the
// returned stop function is called (stop waits for a refresh in flight).
func (e *Engine) startLockRefresh(ctx context.Context) (stop func()) {
	held := e.held
	interval := e.LockRefresh
	if held == nil || interval <= 0 {
		return func() {}
	}
	rctx, cancel := context.WithCancel(withoutCancel(ctx))
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-rctx.Done():
				return
			case <-ticker.C:
				if err := e.Remote.Run(rctx, refreshScript(held.path, held.token)); err != nil && rctx.Err() == nil {
					e.printf("Warning: refreshing the deployment lock failed: %v\n", err)
				}
			}
		}
	}()
	return func() {
		cancel()
		wg.Wait()
	}
}

// LockStatus describes the lock for `unlock` and `doctor`.
type LockStatus struct {
	Held bool      `json:"held"`
	Info *LockInfo `json:"info"`
	// Age is the time since the lock was taken or last refreshed, measured
	// with the deploy host's clock.
	Age time.Duration `json:"-"`
	// AgeSeconds is Age in whole seconds (-1 when unknown).
	AgeSeconds int64 `json:"age_seconds"`
	// Stale reports an age of at least StaleLockAge.
	Stale bool `json:"stale"`
}

// LockAgeLines print the age facts of the lock directory "$lock" (which
// must exist) as key=value lines, using the deploy host's clock: lock_now
// (date +%s), lock_mtime (GNU stat, else BSD stat), and lock_old (find
// -mmin, the fallback when stat is unavailable). Shared with doctor.
func LockAgeLines() []string {
	return []string{
		`printf 'lock_now=%s\n' "$(date +%s 2>/dev/null || true)"`,
		`printf 'lock_mtime=%s\n' "$(stat -c %Y "$lock" 2>/dev/null || stat -f %m "$lock" 2>/dev/null || true)"`,
		`if test -n "$(find "$lock" -prune -mmin +` + strconv.Itoa(int(StaleLockAge/time.Minute)) + ` 2>/dev/null)"; then echo lock_old=yes; else echo lock_old=no; fi`,
	}
}

// LockAge evaluates LockAgeLines facts: the age in seconds (-1 when the
// clock values are unusable) and whether the lock is stale.
func LockAge(now, mtime, old string) (ageSeconds int64, stale bool) {
	n, err1 := strconv.ParseInt(strings.TrimSpace(now), 10, 64)
	m, err2 := strconv.ParseInt(strings.TrimSpace(mtime), 10, 64)
	if err1 != nil || err2 != nil || n <= 0 || m <= 0 {
		return -1, strings.TrimSpace(old) == "yes"
	}
	age := n - m
	if age < 0 {
		age = 0
	}
	return age, age >= int64(StaleLockAge/time.Second)
}

// ReadLock inspects the lock without changing it.
func (e *Engine) ReadLock(ctx context.Context, l *Layout) (*LockStatus, error) {
	lines := []string{
		"set +xv",
		"set -eu",
		"lock=" + shell.Quote(l.Lock()),
		`echo ` + lockMarker,
		`if ! test -d "$lock"; then echo free; exit 0; fi`,
		`echo held`,
	}
	lines = append(lines, LockAgeLines()...)
	lines = append(lines,
		`echo @@info`,
		`if test -f "$lock/info" && ! test -L "$lock/info"; then head -c 4096 "$lock/info"; echo; fi`,
	)
	out, err := e.Remote.Capture(ctx, shell.Script(lines...))
	if err != nil {
		return nil, err
	}
	body := FromMarker(out, lockMarker)
	head, info, _ := strings.Cut(body, "\n@@info\n")
	state, facts, _ := strings.Cut(head, "\n")
	switch strings.TrimSpace(state) {
	case "free":
		return &LockStatus{AgeSeconds: -1}, nil
	case "held":
	default:
		return nil, fmt.Errorf("remote host returned an invalid lock response")
	}
	m := kv(facts)
	status := &LockStatus{Held: true, Info: parseLockInfo(info)}
	status.AgeSeconds, status.Stale = LockAge(m["lock_now"], m["lock_mtime"], m["lock_old"])
	if status.AgeSeconds >= 0 {
		status.Age = time.Duration(status.AgeSeconds) * time.Second
	}
	return status, nil
}

// ForceUnlockResult reports what `depper unlock` did.
type ForceUnlockResult struct {
	Removed bool        `json:"removed"`
	Lock    *LockStatus `json:"lock"`
}

// BreakLock removes the lock. Without force it refuses a lock that was taken
// or refreshed less than StaleLockAge ago (by the host's clock). It removes
// only the lock it inspected: if the lock changed hands in between, it
// fails instead.
func (e *Engine) BreakLock(ctx context.Context, force bool) (*ForceUnlockResult, error) {
	l, err := e.ProbeForChange(ctx)
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
		return nil, fmt.Errorf("the lock is held by %s and was refreshed less than %d minutes ago; wait for that operation or use --force", status.Info.describe(), int(StaleLockAge/time.Minute))
	}
	token := ""
	if status.Info != nil {
		token = status.Info.token
	}
	if _, err := e.Remote.Capture(ctx, releaseScript(l.Lock(), token)); err != nil {
		return nil, err
	}
	return &ForceUnlockResult{Removed: true, Lock: status}, nil
}
