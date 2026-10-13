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
	revertScript = "revert.sh"
)

// guardMarkers are the markers that mean a change is armed and not yet
// verified (or not yet reverted).
var guardMarkers = []string{markHadPrev, markHadNone, markWasInactive, markAddedPorts}

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

// revertScriptBody is the script the timer runs ("timer") and host init
// runs itself ("now"). It records who ran it.
func (r *Runner) revertScriptBody(g *guard) string {
	d := r.guardDir(g.unit)
	lines := []string{
		"set +e",
		"d=" + shell.Quote(d),
		`depper_who=${1:-now}`,
		`if test "$depper_who" = timer; then date +%s > "$d/` + markStarted + `"; fi`,
	}
	lines = append(lines, g.revert...)
	lines = append(lines, `printf '%s %s\n' "$depper_who" "$(date +%s)" > "$d/`+markReverted+`"`)
	return shell.Script(lines...)
}

// pendingLines are root lines that fail when a revert of unit is still
// pending (an armed timer or markers left by an interrupted run).
func (r *Runner) pendingCheck(unit string) []string {
	d := shell.Quote(r.guardDir(unit))
	var tests []string
	for _, m := range guardMarkers {
		tests = append(tests, `test -e `+d+`/`+m)
	}
	return []string{
		`if test "$(systemctl show -p SubState --value ` + shell.Quote(unit+".timer") + ` 2>/dev/null)" = waiting || ` + strings.Join(tests, " || ") + `; then echo "the revert of an earlier, interrupted host init (` + unit + `) is still pending; run host init again to finish it first" >&2; exit 1; fi`,
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
	lines := []string{"set -eu", "umask 022", "d=" + shell.Quote(d), `mkdir -p "$d"`}
	lines = append(lines, r.pendingCheck(g.unit)...)
	lines = append(lines,
		// A finished (elapsed) timer of an earlier run still holds the
		// unit name; it has nothing left to do.
		`if systemctl show -p LoadState --value `+timer+` 2>/dev/null | grep -qx loaded; then echo "clearing the finished `+g.unit+`.timer of an earlier run"; systemctl stop `+timer+` >/dev/null 2>&1 || true; fi`,
		`systemctl reset-failed `+timer+` `+service+` >/dev/null 2>&1 || true`,
		`rm -f "$d/`+markStarted+`" "$d/`+markReverted+`" "$d/`+markReloaded+`"`,
		`printf '%s' `+shell.Quote(r.revertScriptBody(g))+` > "$d/`+revertScript+`"`,
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
	stepDisarm    = "revert-disarm"
	revertElapsed = "revert-elapsed"
)

// disarm stops the timer, then checks whether the revert service ever
// started; elapsed reports that it did (the timer undid the change, or is
// undoing it). On success the markers are removed.
func (r *Runner) disarm(ctx context.Context, admin *transport.Remote, g *guard) (elapsed bool, err error) {
	d := r.guardDir(g.unit)
	timer, service := shell.Quote(g.unit+".timer"), shell.Quote(g.unit+".service")
	var markers []string
	for _, m := range append(append([]string{}, guardMarkers...), markPrev, markReloaded, revertScript) {
		markers = append(markers, `"$d/`+m+`"`)
	}
	lines := []string{
		"d=" + shell.Quote(d),
		`systemctl stop ` + timer + ` >/dev/null 2>&1 || true`,
		`if test "$(systemctl show -p SubState --value ` + timer + ` 2>/dev/null)" = waiting; then echo "could not stop ` + g.unit + `.timer" >&2; exit 1; fi`,
		`depper_entered=$(systemctl show -p ActiveEnterTimestampMonotonic --value ` + service + ` 2>/dev/null || true)`,
		`if test -e "$d/` + markStarted + `" || { test -n "$depper_entered" && test "$depper_entered" != 0; }; then echo ` + shell.Quote(doneMarker+revertElapsed) + `; echo "` + g.unit + `.service had already started" >&2; exit 1; fi`,
		`rm -f ` + strings.Join(markers, " "),
	}
	done, err := r.applySteps(ctx, admin, []*Step{{ID: stepDisarm, lines: lines}})
	if err == nil {
		return false, nil
	}
	return contains(done, revertElapsed), err
}

// finish disarms g after the change was verified. A failed disarm is
// reported loudly: the timer would undo the verified change.
func (r *Runner) finish(ctx context.Context, admin *transport.Remote, g *guard) error {
	elapsed, err := r.disarm(ctx, admin, g)
	switch {
	case err == nil:
		r.printf("  verified; disarmed %s.timer\n", g.unit)
		return nil
	case elapsed:
		return fmt.Errorf("%s: the change was verified, but %s.timer had already started the revert (the checks took longer than %s), so the change was undone; run host init again", g.name, g.unit, r.revertAfter())
	}
	r.printf("WARNING: %s: could not disarm %s.timer (%v); it will undo the change at %s\n", g.name, g.unit, err, clock(g.deadline(r)))
	return fmt.Errorf("%s: the change was verified, but disarming %s.timer failed (%v), so it will undo the change at %s; run host init again, or stop the timer yourself with `sudo systemctl stop %s.timer`", g.name, g.unit, err, clock(g.deadline(r)), g.unit)
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
		elapsed, derr := r.disarm(ctx, admin, g)
		switch {
		case elapsed:
			return fmt.Errorf("%s: %v; rolled back the change, and %s.timer had also started the same revert (it does nothing twice)", g.name, cause, g.unit)
		case derr != nil:
			return fmt.Errorf("%s: %v; rolled back the change, but disarming %s.timer failed (%v); it will run the revert again at %s, which then does nothing", g.name, cause, g.unit, derr, clock(g.deadline(r)))
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
				how = "the change was undone by a revert outside the timer"
			}
			return fmt.Errorf("%s: %v; %s at %s; a new %s login works again", g.name, cause, how, clock(when), userOf(admin.Target))
		}
		if !time.Now().Before(limit) {
			if err != nil {
				return fmt.Errorf("%s: %v; the immediate rollback failed (%v); %s.timer on the host was due to undo the change at %s, but a new %s login still fails (%v): use the provider console to check %s and %s", g.name, cause, rerr, g.unit, clock(at), userOf(admin.Target), err, g.unit, r.guardDir(g.unit))
			}
			return fmt.Errorf("%s: %v; the immediate rollback failed (%v); %s.timer was due at %s but has not run the revert, although a new %s login works again: run host init again (it finishes the revert first)", g.name, cause, rerr, g.unit, clock(at), userOf(admin.Target))
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
	file := shell.Quote(r.raw(SSHDropIn))
	reload := `systemctl reload ssh.service 2>/dev/null || systemctl reload sshd.service 2>/dev/null || true`
	if sshdUnit != "" {
		reload = `systemctl reload ` + shell.Quote(sshdUnit+".service") + ` || ` + reload
	}
	reload = `if test "$depper_who" = timer || test -f "$d/` + markReloaded + `"; then ` + reload + `; fi; rm -f "$d/` + markReloaded + `"`
	return &guard{
		name: "--harden-ssh",
		unit: sshRevertUnit,
		prepare: []string{
			`if test -f ` + file + `; then cp -p ` + file + ` "$d/` + markPrev + `"; : > "$d/` + markHadPrev + `"; else : > "$d/` + markHadNone + `"; fi`,
		},
		revert: []string{
			`if test -f "$d/` + markHadPrev + `"; then`,
			`  cp -p "$d/` + markPrev + `" ` + file + `.depper-tmp && mv -f ` + file + `.depper-tmp ` + file + ` && rm -f "$d/` + markHadPrev + `" "$d/` + markPrev + `"`,
			`  ` + reload,
			`elif test -f "$d/` + markHadNone + `"; then`,
			`  rm -f ` + file + ` && rm -f "$d/` + markHadNone + `"`,
			`  ` + reload,
			`fi`,
		},
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
			return fmt.Errorf("--harden-ssh: %s cannot log in with its key (%v), and reverting %s failed: %v; %s.timer reverts it at %s", o.User, err, SSHDropIn, rerr, g.unit, clock(g.deadline(r)))
		}
		if _, derr := r.disarm(ctx, admin, g); derr != nil {
			return fmt.Errorf("--harden-ssh: %s cannot log in with its key over a new connection (%v); reverted %s and did not reload sshd, but disarming %s.timer failed (%v; its revert does nothing more)", o.User, err, SSHDropIn, g.unit, derr)
		}
		return fmt.Errorf("--harden-ssh: %s cannot log in with its key over a new connection (%v); reverted %s and did not reload sshd", o.User, err, SSHDropIn)
	}
	if err := r.runRoot(ctx, admin, []string{
		`if systemctl is-active --quiet ssh.service; then depper_unit=ssh.service; elif systemctl is-active --quiet sshd.service; then depper_unit=sshd.service; else echo "cannot find the running sshd service (ssh.service or sshd.service)" >&2; exit 1; fi`,
		`: > ` + shell.Quote(r.guardDir(g.unit)+"/"+markReloaded),
		`systemctl reload "$depper_unit"`,
	}); err != nil {
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
			if st.Markers {
				what += ", revert markers present"
			}
			out = append(out, what)
		}
	}
	return strings.Join(out, "; ")
}

// finishPending completes guarded changes an interrupted host init left
// armed: a waiting timer has its revert service started now (oneshot, so
// systemctl waits for it); markers without a timer run the revert script
// directly. It reports whether anything was pending.
func (r *Runner) finishPending(ctx context.Context, admin *transport.Remote, f *Facts) (bool, error) {
	var lines []string
	for _, unit := range guardUnits {
		st := f.Guards[unit]
		if !st.Pending() {
			continue
		}
		d := shell.Quote(r.guardDir(unit))
		what := map[string]string{sshRevertUnit: "--harden-ssh", firewallRevertUnit: "--firewall"}[unit]
		r.printf("Finishing an interrupted host init: its %s change was never verified, so %s reverts it now (timer %s).\n", what, unit, shellName(st.Timer))
		lines = append(lines,
			`if test "$(systemctl show -p SubState --value `+shell.Quote(unit+".timer")+` 2>/dev/null)" = waiting; then systemctl start `+shell.Quote(unit+".service")+`; fi`,
			`systemctl stop `+shell.Quote(unit+".timer")+` >/dev/null 2>&1 || true`,
		)
		var tests []string
		for _, m := range guardMarkers {
			tests = append(tests, `test -e `+d+`/`+m)
		}
		lines = append(lines,
			`if `+strings.Join(tests, " || ")+`; then`,
			`  test -f `+d+`/`+revertScript+` || { echo "`+unit+`: revert markers without `+revertScript+` in `+r.guardDir(unit)+`; check them by hand" >&2; exit 1; }`,
			`  /bin/sh `+d+`/`+revertScript+` now`,
			`fi`,
			`if `+strings.Join(tests, " || ")+`; then echo "`+unit+`: the revert left markers in `+r.guardDir(unit)+`" >&2; exit 1; fi`,
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
