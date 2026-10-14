package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/frgmt0/depper/internal/shell"
	"github.com/frgmt0/depper/internal/transport"
)

// Transient systemd units that undo a guarded change on their own.
const (
	sshRevertUnit      = "depper-ssh-revert"
	firewallRevertUnit = "depper-firewall-revert"
)

// guardUnits are the revert units, in the order host init finishes them.
var guardUnits = []string{firewallRevertUnit, sshRevertUnit}

// guardStateDir is where a guarded change keeps its revert script and the
// markers that say what to undo (root-owned, mode 755: the files hold no
// secrets, and the deploy user may read when a revert ran).
const guardStateDir = "/var/lib/depper/guard"

// Marker files in a guard's state directory. The revert script acts only on
// the markers that exist and removes each one once handled, so running it
// twice (by the timer and by host init) is harmless.
const (
	// markHadPrev: a drop-in existed before; its copy is in "prev".
	markHadPrev = "had-prev"
	markPrev    = "prev"
	// markHadNone: there was no drop-in; the revert removes it.
	markHadNone = "had-none"
	// markWasInactive: the firewall was off; the revert turns it off.
	markWasInactive = "was-inactive"
	// markAddedPorts: TCP ports the revert removes again, one per line.
	markAddedPorts = "added-ports"
	// markStarted is written when the timer starts the revert;
	// markReverted ("<who> <unix time>") when any revert finished.
	markStarted  = "started"
	markReverted = "reverted"
	// markReloaded is written just before sshd reloads the drop-in; a
	// revert by host init reloads sshd only then (the timer always does).
	markReloaded = "reloaded"
	// markDisarmed is written when host init disarms the guard after the
	// change was verified; a revert started by the timer or at boot then
	// does nothing.
	markDisarmed = "disarmed"
	// markWasEnabled: firewalld was enabled (but not running) before; the
	// revert stops it without disabling it.
	markWasEnabled = "was-enabled"
	// markEnabledUnit: the step enabled the firewall service at boot
	// (ufw.service on Arch); the revert disables it again.
	markEnabledUnit = "enabled-unit"
	// lockDir serializes the revert script and disarm (mkdir is atomic).
	lockDir      = "lock"
	revertScript = "revert.sh"
	// bootScript runs every guard's revert at boot (transient timers do not
	// survive a reboot); bootUnit is the system service that runs it.
	bootScript = "boot.sh"
	bootUnit   = "depper-guard-revert.service"
)

// lockLines take the guard lock in $d (waiting up to 90 s, then taking over
// a lock left by a killed process) and release it when the script exits.
func lockLines() []string {
	return []string{
		`depper_n=0`,
		`while ! mkdir "$d/` + lockDir + `" 2>/dev/null; do depper_n=$((depper_n + 1)); if test "$depper_n" -ge 90; then echo "taking over the stale lock $d/` + lockDir + `" >&2; break; fi; sleep 1; done`,
		`trap 'rmdir "$d/` + lockDir + `" 2>/dev/null' EXIT`,
	}
}

// guardMarkers are the markers that mean a change is armed and not yet
// verified (or not yet reverted).
var guardMarkers = []string{markHadPrev, markHadNone, markWasInactive, markAddedPorts, markEnabledUnit}

// guard is a change that could lock everyone out of the host (sshd
// hardening, the firewall). Before it is applied, host init records what to
// undo (marker files) and arms a transient systemd timer that runs the
// revert script after Runner.revertDelay, so the change undoes itself even
// when no new login is possible and even when depper is interrupted. It is
// disarmed only after new admin and deploy logins succeed.
type guard struct {
	// name is the flag the change belongs to, for messages.
	name string
	// unit is the transient unit name (unit.timer and unit.service).
	unit string
	// prepare are root lines that write the markers ($d is the state
	// directory).
	prepare []string
	// revert is the body of the revert script; it must act only on
	// markers and remove each marker it handled.
	revert []string
	// armedAt is when the timer was armed (after systemd-run returned).
	armedAt time.Time
	// armedWall is armedAt on the local monotonic clock, for waiting
	// (Runner.Now may be fixed in tests).
	armedWall time.Time
}

func (r *Runner) revertAfter() time.Duration {
	if r.revertDelay > 0 {
		return r.revertDelay
	}
	return RevertDelay
}

func (r *Runner) revertWait() time.Duration {
	if r.revertGrace > 0 {
		return r.revertGrace
	}
	return 15 * time.Second
}

func (r *Runner) pollEvery() time.Duration {
	if r.retryDelay > 0 {
		return r.retryDelay
	}
	return 5 * time.Second
}

// dir is the guard's state directory on the host.
func (r *Runner) guardDir(unit string) string { return r.raw(guardStateDir + "/" + unit) }

// deadline is when the armed revert runs.
func (g *guard) deadline(r *Runner) time.Time { return g.armedAt.Add(r.revertAfter()) }

func clock(t time.Time) string { return t.Format("15:04:05 MST") }

// revertScriptBody is the revert script. Its argument says who runs it:
// "timer" (the armed timer), "boot" (the boot service after a reboot),
// "pending" (host init finishing an interrupted run) or "now" (host init
// rolling back). It holds the guard lock while it works, does nothing when
// started automatically after the guard was disarmed, and exits 1 without
// writing the reverted marker unless every undo step succeeded (markers of
// failed steps stay for the next attempt).
func (r *Runner) revertScriptBody(g *guard) string {
	d := r.guardDir(g.unit)
	lines := []string{
		"set +e",
		"d=" + shell.Quote(d),
		`depper_who=${1:-now}`,
	}
	lines = append(lines, lockLines()...)
	lines = append(lines,
		`case "$depper_who" in timer|boot) if test -f "$d/`+markDisarmed+`"; then exit 0; fi; date +%s > "$d/`+markStarted+`" ;; esac`,
		`depper_fail=0`,
	)
	lines = append(lines, g.revert...)
	var tests []string
	for _, m := range guardMarkers {
		tests = append(tests, `test -e "$d/`+m+`"`)
	}
	lines = append(lines,
		`if `+strings.Join(tests, " || ")+`; then depper_fail=1; fi`,
		`if test "$depper_fail" != 0; then echo "`+g.unit+`: the revert did not complete; the markers of what is left to undo stay in $d" >&2; exit 1; fi`,
		`printf '%s %s\n' "$depper_who" "$(date +%s)" > "$d/`+markReverted+`"`,
	)
	return shell.Script(lines...)
}

// bootScriptBody runs every guard's revert script at boot.
func (r *Runner) bootScriptBody() string {
	return shell.Script(
		"for s in "+shell.Quote(r.raw(guardStateDir))+"/*/"+revertScript+"; do",
		`  test -f "$s" && /bin/sh "$s" boot`,
		"done",
		"exit 0",
	)
}

// bootUnitContent is the system service that runs bootScript at boot,
// after sshd and the firewall started.
func (r *Runner) bootUnitContent() string {
	return "[Unit]\nDescription=depper: undo guarded changes a reboot left unverified\nAfter=network.target ssh.service sshd.service ufw.service firewalld.service\n\n[Service]\nType=oneshot\nExecStart=/bin/sh " + r.raw(guardStateDir) + "/" + bootScript + "\n\n[Install]\nWantedBy=multi-user.target\n"
}

// pendingLines are root lines that fail when a revert of unit is still
// pending (an armed timer, a running revert, or markers left by an
// interrupted run).
func (r *Runner) pendingCheck(unit string) []string {
	d := shell.Quote(r.guardDir(unit))
	var tests []string
	for _, m := range guardMarkers {
		tests = append(tests, `test -e `+d+`/`+m)
	}
	return []string{
		`if test "$(systemctl show -p SubState --value ` + shell.Quote(unit+".timer") + ` 2>/dev/null)" = waiting || test "$(systemctl show -p ActiveState --value ` + shell.Quote(unit+".service") + ` 2>/dev/null)" = activating || ` + strings.Join(tests, " || ") + `; then echo "the revert of an earlier, interrupted host init (` + unit + `) is still pending; run host init again to finish it first" >&2; exit 1; fi`,
	}
}

// arm records the markers, writes the revert script and starts the
// transient timer. An earlier revert that is still pending is refused here:
// Init finishes it before planning (finishPending).
func (r *Runner) arm(ctx context.Context, admin *transport.Remote, g *guard) error {
	secs := int(r.revertAfter() / time.Second)
	if secs < 1 {
		secs = 1
	}
	d := r.guardDir(g.unit)
	timer, service := shell.Quote(g.unit+".timer"), shell.Quote(g.unit+".service")
	var markers []string
	for _, m := range append(append([]string{}, guardMarkers...), markPrev, revertScript) {
		markers = append(markers, `"$d/`+m+`"`)
	}
	base := r.raw(guardStateDir)
	unitFile := r.raw("/etc/systemd/system/" + bootUnit)
	lines := []string{"set -eu", "umask 022", "d=" + shell.Quote(d), `mkdir -p "$d"`}
	lines = append(lines, r.pendingCheck(g.unit)...)
	lines = append(lines,
		// A finished (elapsed) timer of an earlier run still holds the
		// unit name; it has nothing left to do.
		`if systemctl show -p LoadState --value `+timer+` 2>/dev/null | grep -qx loaded; then echo "clearing the finished `+g.unit+`.timer of an earlier run"; systemctl stop `+timer+` >/dev/null 2>&1 || true; fi`,
		`systemctl reset-failed `+timer+` `+service+` >/dev/null 2>&1 || true`,
		// No revert runs now (checked above), so a lock left behind is stale.
		`rmdir "$d/`+lockDir+`" 2>/dev/null || true`,
		`rm -f "$d/`+markStarted+`" "$d/`+markReverted+`" "$d/`+markReloaded+`" "$d/`+markDisarmed+`" "$d/`+markWasEnabled+`"`,
		`printf '%s' `+shell.Quote(r.revertScriptBody(g))+` > "$d/`+revertScript+`.tmp"`,
		`mv -f "$d/`+revertScript+`.tmp" "$d/`+revertScript+`"`,
		// Transient timers do not survive a reboot: a boot service runs
		// the revert of a change a reboot left unverified.
		`printf '%s' `+shell.Quote(r.bootScriptBody())+` > `+shell.Quote(base+"/"+bootScript+".tmp"),
		`mv -f `+shell.Quote(base+"/"+bootScript+".tmp")+` `+shell.Quote(base+"/"+bootScript),
		`mkdir -p `+shell.Quote(r.raw("/etc/systemd/system")),
		`if ! test -f `+shell.Quote(unitFile)+` || test "$(cat `+shell.Quote(unitFile)+`)" != `+shell.Quote(strings.TrimSuffix(r.bootUnitContent(), "\n"))+`; then printf '%s' `+shell.Quote(r.bootUnitContent())+` > `+shell.Quote(unitFile+".tmp")+`; mv -f `+shell.Quote(unitFile+".tmp")+` `+shell.Quote(unitFile)+`; systemctl daemon-reload; fi`,
		`systemctl is-enabled --quiet `+bootUnit+` 2>/dev/null || systemctl enable --quiet `+bootUnit,
	)
	lines = append(lines, g.prepare...)
	lines = append(lines,
		`if ! depper_out=$(systemd-run --quiet --collect --unit=`+shell.Quote(g.unit)+` --property=Type=oneshot --timer-property=AccuracySec=1s --timer-property=RandomizedDelaySec=0 --on-active=`+strconv.Itoa(secs)+`s /bin/sh "$d/`+revertScript+`" timer 2>&1); then`,
		`  if rm -f `+strings.Join(markers, " ")+`; then depper_clean="it had written the revert script and markers to $d and removed them again"; else depper_clean="it had written the revert script and markers to $d and could NOT remove them: remove them by hand, or the next host init reverts the unchanged host"; fi`,
		`  printf 'systemd-run failed (%s), so host init stopped before changing anything else: %s\n' "$depper_out" "$depper_clean" >&2`,
		`  exit 1`,
		`fi`,
	)
	if err := r.runRoot(ctx, admin, lines); err != nil {
		return fmt.Errorf("%s: arming the automatic revert failed: %w", g.name, err)
	}
	g.armedAt, g.armedWall = r.now(), time.Now()
	r.printf("  armed %s.timer: it undoes this change at %s unless new logins succeed first\n", g.unit, clock(g.deadline(r)))
	return nil
}

const (
	stepDisarm       = "revert-disarm"
	revertElapsed    = "revert-elapsed"
	revertIncomplete = "revert-incomplete"
	timerStopped     = "revert-timer-stopped"
)

// disarmResult is what disarm found on the host.
type disarmResult struct {
	// stopped: the timer is stopped (it will not fire any more).
	stopped bool
	// elapsed: the revert had started automatically (timer or boot);
	// incomplete: it did not finish (markers are left).
	elapsed, incomplete bool
}

// disarm stops the timer, then takes the guard lock (so a revert that has
// started finishes first) and checks whether the revert ever started
// automatically. If it did not, it marks the guard disarmed (a revert
// started later does nothing) and removes the markers.
func (r *Runner) disarm(ctx context.Context, admin *transport.Remote, g *guard) (disarmResult, error) {
	d := r.guardDir(g.unit)
	timer := shell.Quote(g.unit + ".timer")
	var markers []string
	for _, m := range append(append([]string{}, guardMarkers...), markPrev, markReloaded, markWasEnabled, "zone", revertScript) {
		markers = append(markers, `"$d/`+m+`"`)
	}
	lines := []string{"d=" + shell.Quote(d)}
	lines = append(lines,
		`systemctl stop `+timer+` >/dev/null 2>&1 || true`,
		`if test "$(systemctl show -p SubState --value `+timer+` 2>/dev/null)" = waiting; then echo "could not stop `+g.unit+`.timer" >&2; exit 1; fi`,
		`echo `+shell.Quote(doneMarker+timerStopped),
	)
	lines = append(lines, lockLines()...)
	lines = append(lines,
		`if test -e "$d/`+markStarted+`"; then echo `+shell.Quote(doneMarker+revertElapsed)+`; if ! test -e "$d/`+markReverted+`"; then echo `+shell.Quote(doneMarker+revertIncomplete)+`; fi; echo "`+g.unit+` had already started the revert" >&2; exit 1; fi`,
		`: > "$d/`+markDisarmed+`"`,
		`rm -f `+strings.Join(markers, " "),
	)
	done, err := r.applySteps(ctx, admin, []*Step{{ID: stepDisarm, lines: lines}})
	res := disarmResult{stopped: err == nil || contains(done, timerStopped), elapsed: contains(done, revertElapsed), incomplete: contains(done, revertIncomplete)}
	return res, err
}

// finish disarms g after the change was verified. A failed disarm is
// reported loudly: the timer, or the next host init, would undo the
// verified change.
func (r *Runner) finish(ctx context.Context, admin *transport.Remote, g *guard) error {
	res, err := r.disarm(ctx, admin, g)
	d := r.guardDir(g.unit)
	switch {
	case err == nil:
		r.printf("  verified; disarmed %s.timer\n", g.unit)
		return nil
	case res.elapsed && res.incomplete:
		return fmt.Errorf("%s: the change was verified, but %s had already started the revert (the checks took longer than %s) and it did not complete; the markers of what is left to undo are in %s, and the next host init finishes the revert first", g.name, g.unit, r.revertAfter(), d)
	case res.elapsed:
		return fmt.Errorf("%s: the change was verified, but %s had already started the revert (the checks took longer than %s), so the change was undone; run host init again", g.name, g.unit, r.revertAfter())
	case res.stopped:
		r.printf("WARNING: %s: stopped %s.timer, but could not clear the revert markers (%v)\n", g.name, g.unit, err)
		return fmt.Errorf("%s: the change was verified and %s.timer is stopped, but clearing the revert markers in %s failed (%v); the next host init would undo the change: remove had-prev, prev, had-none, was-inactive, added-ports and enabled-unit there yourself to keep it", g.name, g.unit, d, err)
	}
	r.printf("WARNING: %s: could not disarm %s.timer (%v); it will undo the change at %s\n", g.name, g.unit, err, clock(g.deadline(r)))
	return fmt.Errorf("%s: the change was verified, but disarming %s.timer failed (%v), so it will undo the change at %s; run host init again, or to keep the change stop the timer yourself (`sudo systemctl stop %s.timer`) and remove the markers had-prev, prev, had-none, was-inactive, added-ports and enabled-unit in %s", g.name, g.unit, err, clock(g.deadline(r)), g.unit, d)
}

// e2eLockout is a test-only hook (see docs/development.md): with
// DEPPER_E2E=1 and DEPPER_E2E_LOCKOUT_ADMIN=1, the admin login after the
// sshd reload is reported as failed and no immediate rollback is tried, so
// the end-to-end test exercises the revert timer on a real host.
func e2eLockout() bool {
	return os.Getenv("DEPPER_E2E") == "1" && os.Getenv("DEPPER_E2E_LOCKOUT_ADMIN") == "1"
}

var errE2ELockout = errors.New("DEPPER_E2E_LOCKOUT_ADMIN: the admin login is treated as failed (test hook)")

// rollback undoes g after a failure (cause): at once through the admin
// connection when that still works (then the timer is disarmed), otherwise
// by waiting for the armed timer, polling for the marker its revert writes.
// The returned error says exactly what happened.
func (r *Runner) rollback(ctx context.Context, admin *transport.Remote, g *guard, cause error) error {
	r.printf("  %v\n  rolling back...\n", cause)
	var rerr error
	if errors.Is(cause, errE2ELockout) {
		rerr = errE2ELockout
	} else {
		rerr = r.runRoot(ctx, admin, []string{"/bin/sh " + shell.Quote(r.guardDir(g.unit)+"/"+revertScript) + " now"})
	}
	if rerr == nil {
		res, derr := r.disarm(ctx, admin, g)
		switch {
		case res.elapsed:
			return fmt.Errorf("%s: %v; rolled back the change, and %s had also started the same revert (the two ran one after the other; the second had nothing left to undo)", g.name, cause, g.unit)
		case derr != nil && res.stopped:
			return fmt.Errorf("%s: %v; rolled back the change and stopped %s.timer, but clearing its state in %s failed (%v); nothing is left to undo", g.name, cause, g.unit, r.guardDir(g.unit), derr)
		case derr != nil:
			return fmt.Errorf("%s: %v; rolled back the change, but stopping %s.timer failed (%v); it will run the revert again at %s, which then has nothing left to undo", g.name, cause, g.unit, derr, clock(g.deadline(r)))
		}
		return fmt.Errorf("%s: %v; rolled back the change and disarmed %s.timer", g.name, cause, g.unit)
	}
	at := g.deadline(r)
	r.printf("  the immediate rollback failed (%v)\n  waiting for %s.timer, armed on the host, to undo the change at %s...\n", rerr, g.unit, clock(at))
	limit := g.armedWall.Add(r.revertAfter() + r.revertWait())
	for {
		who, when, err := r.revertMarker(ctx, admin, g)
		if err == nil && who != "" {
			how := "the immediate rollback failed (" + rerr.Error() + "), so " + g.unit + ".timer on the host undid the change"
			if who != "timer" {
				how = "the change was undone by a revert outside the timer (" + who + ")"
			}
			return fmt.Errorf("%s: %v; %s at %s; a new %s login works again", g.name, cause, how, clock(when), userOf(admin.Target))
		}
		if !time.Now().Before(limit) {
			if err != nil {
				return fmt.Errorf("%s: %v; the immediate rollback failed (%v); %s.timer on the host was due to undo the change at %s, but a new %s login still fails (%v): use the provider console to check %s and %s", g.name, cause, rerr, g.unit, clock(at), userOf(admin.Target), err, g.unit, r.guardDir(g.unit))
			}
			return fmt.Errorf("%s: %v; the immediate rollback failed (%v); %s.timer was due at %s, but no completed revert is recorded in %s (it may not have run, or failed: see `journalctl -u %s.service` on the host), although a new %s login works again: run host init again (it finishes the revert first)", g.name, cause, rerr, g.unit, clock(at), r.guardDir(g.unit), g.unit, userOf(admin.Target))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: %v; the immediate rollback failed (%v); %s.timer on the host undoes the change at %s (host init was interrupted before it could check)", g.name, cause, rerr, g.unit, clock(at))
		case <-time.After(r.pollEvery()):
		}
	}
}

// revertMarker reads the "reverted" marker over a new admin connection:
// who ran the revert and when (host clock). who is "" while none ran.
func (r *Runner) revertMarker(ctx context.Context, admin *transport.Remote, g *guard) (who string, when time.Time, err error) {
	lines, err := r.asRoot([]string{`cat ` + shell.Quote(r.guardDir(g.unit)+"/"+markReverted) + ` 2>/dev/null || true`})
	if err != nil {
		return "", time.Time{}, err
	}
	rem := *admin
	rem.Target.Options = append(append([]string{}, admin.Target.Options...), "ConnectTimeout=10")
	out, err := rem.Capture(ctx, shell.Script(lines...))
	if err != nil {
		return "", time.Time{}, err
	}
	f := strings.Fields(out)
	if len(f) < 2 {
		return "", time.Time{}, nil
	}
	sec, perr := strconv.ParseInt(f[len(f)-1], 10, 64)
	if perr != nil {
		return "", time.Time{}, nil
	}
	return f[len(f)-2], time.Unix(sec, 0).In(r.now().Location()), nil
}

func userOf(t transport.Target) string {
	u, _ := t.SplitHost()
	return u
}

// verifyBoth opens new admin and deploy connections.
func (r *Runner) verifyBoth(ctx context.Context, adminT, deployT transport.Target, lockout bool) error {
	if err := r.verifyLoginRetry(ctx, deployT); err != nil {
		return fmt.Errorf("a new login as %s failed (%v)", userOf(deployT), err)
	}
	if lockout {
		return fmt.Errorf("a new login as %s failed (%w)", userOf(adminT), errE2ELockout)
	}
	if err := r.verifyLoginRetry(ctx, adminT); err != nil {
		return fmt.Errorf("a new login as %s failed (%v)", userOf(adminT), err)
	}
	return nil
}

// applyFirewall enables the firewall step s as a guarded change.
func (r *Runner) applyFirewall(ctx context.Context, admin *transport.Remote, adminT, deployT transport.Target, s *Step) error {
	r.printf("==> %s\n", s.Title)
	g := &guard{name: "--firewall", unit: firewallRevertUnit, prepare: s.prepare, revert: s.revert}
	if err := r.arm(ctx, admin, g); err != nil {
		return err
	}
	if _, err := r.applySteps(ctx, admin, []*Step{{ID: StepFirewall, lines: s.lines}}); err != nil {
		return r.rollback(ctx, admin, g, fmt.Errorf("configuring the firewall failed: %w", err))
	}
	if r.interruptAt == StepFirewall {
		return errInterrupted
	}
	r.printf("  firewall enabled; checking new connections...\n")
	if err := r.verifyBoth(ctx, adminT, deployT, false); err != nil {
		return r.rollback(ctx, admin, g, fmt.Errorf("%w after enabling the firewall", err))
	}
	return r.finish(ctx, admin, g)
}

// sshGuard is the guard of --harden-ssh: the markers say whether a drop-in
// existed (its copy is kept) or not, and the revert restores or removes it
// and reloads sshd.
func (r *Runner) sshGuard(sshdUnit string) *guard {
	file := r.raw(SSHDropIn)
	f, tmp := shell.Quote(file), shell.Quote(file+".depper-revert.")+`"$$"`
	reload := `depper_sshd_reload`
	return &guard{
		name: "--harden-ssh",
		unit: sshRevertUnit,
		prepare: []string{
			`if test -f ` + f + `; then cp -p ` + f + ` "$d/` + markPrev + `"; : > "$d/` + markHadPrev + `"; else : > "$d/` + markHadNone + `"; fi`,
		},
		// Each marker is removed only after its undo step succeeded. sshd
		// is reloaded when the file was restored and sshd may have loaded
		// it: always when run automatically or for an interrupted run (a
		// restart may have loaded it), on a rollback only after host init
		// reloaded it.
		revert: append(sshdReloadFuncs(sshdUnit),
			`depper_restored=no`,
			`if test -f "$d/`+markHadPrev+`"; then`,
			`  if cp -p "$d/`+markPrev+`" `+tmp+` && mv -f `+tmp+` `+f+`; then rm -f "$d/`+markHadPrev+`" "$d/`+markPrev+`"; depper_restored=yes; else rm -f `+tmp+`; echo "could not restore `+SSHDropIn+` from $d/`+markPrev+`" >&2; depper_fail=1; fi`,
			`elif test -f "$d/`+markHadNone+`"; then`,
			`  if rm -f `+f+`; then rm -f "$d/`+markHadNone+`"; depper_restored=yes; else echo "could not remove `+SSHDropIn+`" >&2; depper_fail=1; fi`,
			`fi`,
			`if test "$depper_restored" = yes; then`,
			`  case "$depper_who" in now) test -f "$d/`+markReloaded+`" && depper_reload=yes || depper_reload=no ;; *) depper_reload=yes ;; esac`,
			`  if test "$depper_reload" = yes; then if `+reload+`; then rm -f "$d/`+markReloaded+`"; else echo "could not reload sshd after restoring `+SSHDropIn+`" >&2; depper_fail=1; fi; else rm -f "$d/`+markReloaded+`"; fi`,
			`fi`,
		),
	}
}

// sshdReloadFuncs define two shell functions. depper_sshd_unit prints the
// unit that runs sshd: the running service (unit, the one the facts found,
// first; then ssh.service, Debian and Ubuntu, and sshd.service, Fedora and
// Arch), else an active ssh.socket or sshd.socket. depper_sshd_reload
// reloads that service; for a socket without a running service every new
// connection starts an sshd that reads the configuration, so there is
// nothing to reload.
func sshdReloadFuncs(unit string) []string {
	units := "ssh.service sshd.service"
	if unit != "" {
		units = shell.Quote(unit+".service") + " " + units
	}
	return []string{
		`depper_sshd_unit() { for depper_u in ` + units + `; do if systemctl is-active --quiet "$depper_u" 2>/dev/null; then echo "$depper_u"; return 0; fi; done; for depper_u in ssh.socket sshd.socket; do if systemctl is-active --quiet "$depper_u" 2>/dev/null; then echo "$depper_u"; return 0; fi; done; return 1; }`,
		`depper_sshd_reload() { depper_u=$(depper_sshd_unit) || { echo "cannot find the running sshd service (ssh.service or sshd.service) or socket (ssh.socket or sshd.socket)" >&2; return 1; }; case "$depper_u" in *.socket) echo "sshd is started per connection by $depper_u: new connections read the configuration, nothing to reload" ;; *) systemctl reload "$depper_u" ;; esac; }`,
	}
}

// hardenSSH writes the sshd drop-in as a guarded change: the revert is
// armed first, the drop-in is validated (sshd -t, sshd -T), a new deploy
// login is proven before sshd reloads, and new deploy and admin logins are
// proven after it. Any failure runs the revert: at once when possible, else
// through the timer.
func (r *Runner) hardenSSH(ctx context.Context, admin *transport.Remote, adminT, deployT transport.Target, o *Options, p *Plan, sshdUnit string) error {
	r.printf("==> Harden sshd\n")
	dir, file := r.raw(sshdDropInD), r.raw(SSHDropIn)
	tmp := file + ".depper-tmp"
	g := r.sshGuard(sshdUnit)
	if err := r.arm(ctx, admin, g); err != nil {
		return err
	}
	revertNow := "/bin/sh " + shell.Quote(r.guardDir(g.unit)+"/"+revertScript) + " now"
	write := []string{
		"set -eu",
		"umask 022",
		"mkdir -p " + shell.Quote(dir),
		"printf '%s' " + shell.Quote(dropInContent(p.hardenRoot)) + " > " + shell.Quote(tmp),
		"chmod 644 " + shell.Quote(tmp),
		"mv -f " + shell.Quote(tmp) + " " + shell.Quote(file),
		`if ! depper_out=$(sshd -t 2>&1); then ` + revertNow + `; printf 'sshd -t rejected the configuration (drop-in reverted): %s\n' "$depper_out" >&2; exit 1; fi`,
		`depper_bad=$(sshd -T 2>/dev/null | awk -v before=` + shell.Quote(p.rootBefore) + ` 'function rank(v) { if (v == "yes") return 0; if (v == "prohibit-password" || v == "without-password") return 1; if (v == "forced-commands-only") return 2; if (v == "no") return 3; return -1 } BEGIN { need = rank(before); if (need < 1) need = 1 } ($1 == "passwordauthentication" && $2 != "no") || ($1 == "kbdinteractiveauthentication" && $2 != "no") || ($1 == "permitrootlogin" && rank($2) < need) { printf "%s %s; ", $1, $2 }')`,
		`if test -n "$depper_bad"; then ` + revertNow + `; printf 'the drop-in does not take effect (sshd -T: %s): an earlier configuration line sets these first; drop-in reverted\n' "$depper_bad" >&2; exit 1; fi`,
	}
	if err := r.runRoot(ctx, admin, write); err != nil {
		return r.rollback(ctx, admin, g, fmt.Errorf("writing the drop-in failed: %w", err))
	}
	if r.interruptAt == StepHardenSSH {
		return errInterrupted
	}
	r.printf("  checking that %s can log in with its key before reloading sshd...\n", o.User)
	if err := r.verifyLogin(ctx, deployT); err != nil {
		if rerr := r.runRoot(ctx, admin, []string{revertNow}); rerr != nil {
			return fmt.Errorf("--harden-ssh: %s cannot log in with its key (%v), and reverting %s failed: %v; %s.timer retries the revert at %s", o.User, err, SSHDropIn, rerr, g.unit, clock(g.deadline(r)))
		}
		res, derr := r.disarm(ctx, admin, g)
		switch {
		case res.elapsed:
			return fmt.Errorf("--harden-ssh: %s cannot log in with its key over a new connection (%v); reverted %s, and %s had also started the same revert", o.User, err, SSHDropIn, g.unit)
		case derr != nil && res.stopped:
			return fmt.Errorf("--harden-ssh: %s cannot log in with its key over a new connection (%v); reverted %s and did not reload sshd; %s.timer is stopped, but clearing its state in %s failed (%v)", o.User, err, SSHDropIn, g.unit, r.guardDir(g.unit), derr)
		case derr != nil:
			return fmt.Errorf("--harden-ssh: %s cannot log in with its key over a new connection (%v); reverted %s and did not reload sshd, but stopping %s.timer failed (%v); its revert at %s has nothing left to undo", o.User, err, SSHDropIn, g.unit, derr, clock(g.deadline(r)))
		}
		return fmt.Errorf("--harden-ssh: %s cannot log in with its key over a new connection (%v); reverted %s and did not reload sshd", o.User, err, SSHDropIn)
	}
	if err := r.runRoot(ctx, admin, append(sshdReloadFuncs(sshdUnit),
		`depper_sshd_unit >/dev/null || { echo "cannot find the running sshd service (ssh.service or sshd.service) or socket (ssh.socket or sshd.socket)" >&2; exit 1; }`,
		`: > `+shell.Quote(r.guardDir(g.unit)+"/"+markReloaded),
		`depper_sshd_reload`,
	)); err != nil {
		return r.rollback(ctx, admin, g, fmt.Errorf("reloading sshd failed: %w", err))
	}
	r.printf("  sshd reloaded; checking new connections...\n")
	if err := r.verifyBoth(ctx, adminT, deployT, e2eLockout()); err != nil {
		return r.rollback(ctx, admin, g, fmt.Errorf("%w after reloading sshd", err))
	}
	return r.finish(ctx, admin, g)
}

// errInterrupted ends host init where Runner.interruptAt says, as if the
// process died there (tests of an interrupted run).
var errInterrupted = errors.New("interrupted (test)")

// pendingGuards describes the guarded changes an interrupted run left
// pending ("" when none).
func pendingGuards(f *Facts) string {
	var out []string
	for _, unit := range guardUnits {
		if st := f.Guards[unit]; st.Pending() {
			what := unit + ".timer " + shellName(st.Timer)
			if st.Service == "activating" {
				what += ", revert running"
			}
			if st.Markers {
				what += ", revert markers present"
			}
			out = append(out, what)
		}
	}
	return strings.Join(out, "; ")
}

// finishPending completes guarded changes an interrupted host init (or a
// reboot) left unverified: a waiting timer has its revert service started
// now (oneshot, so systemctl waits for it), a running revert is waited for,
// and markers left after that run the revert script directly (it holds the
// guard lock, so it never overlaps the service). It reports whether
// anything was pending.
func (r *Runner) finishPending(ctx context.Context, admin *transport.Remote, f *Facts) (bool, error) {
	var lines []string
	for _, unit := range guardUnits {
		st := f.Guards[unit]
		if !st.Pending() {
			continue
		}
		d := shell.Quote(r.guardDir(unit))
		timer, service := shell.Quote(unit+".timer"), shell.Quote(unit+".service")
		what := map[string]string{sshRevertUnit: "--harden-ssh", firewallRevertUnit: "--firewall"}[unit]
		r.printf("Finishing an interrupted host init: its %s change was never verified, so %s reverts it now (timer %s).\n", what, unit, shellName(st.Timer))
		var tests []string
		for _, m := range guardMarkers {
			tests = append(tests, `test -e `+d+`/`+m)
		}
		lines = append(lines,
			`if test "$(systemctl show -p SubState --value `+timer+` 2>/dev/null)" = waiting; then systemctl start `+service+` || echo "`+unit+`.service failed; running the revert script directly" >&2; fi`,
			`systemctl stop `+timer+` >/dev/null 2>&1 || true`,
			`depper_n=0`,
			`while test "$(systemctl show -p ActiveState --value `+service+` 2>/dev/null)" = activating && test "$depper_n" -lt 150; do depper_n=$((depper_n + 1)); sleep 1; done`,
			`if `+strings.Join(tests, " || ")+`; then`,
			`  test -f `+d+`/`+revertScript+` || { echo "`+unit+`: revert markers without `+revertScript+` in `+r.guardDir(unit)+`; check them by hand" >&2; exit 1; }`,
			`  /bin/sh `+d+`/`+revertScript+` pending || { echo "`+unit+`: the revert did not complete; what is left to undo is in `+r.guardDir(unit)+`" >&2; exit 1; }`,
			`fi`,
		)
	}
	if len(lines) == 0 {
		return false, nil
	}
	if err := r.runRoot(ctx, admin, lines); err != nil {
		return true, fmt.Errorf("finishing the revert of an interrupted host init failed: %w", err)
	}
	return true, nil
}
