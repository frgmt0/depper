package host

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/frgmt0/depper/internal/shell"
	"github.com/frgmt0/depper/internal/transport"
)

// Transient systemd units that undo a guarded change on their own.
const (
	sshRevertUnit      = "depper-ssh-revert"
	firewallRevertUnit = "depper-firewall-revert"
)

// guard is a change that could lock everyone out of the host (sshd
// hardening, the firewall). Before it is applied, a transient systemd timer
// is armed on the host that runs revert after Runner.revertDelay, so the
// change undoes itself even when no new login is possible and even when
// depper is interrupted. It is disarmed only after new admin and deploy
// logins succeed.
type guard struct {
	// name is the flag the change belongs to, for messages.
	name string
	// unit is the transient unit name (unit.timer and unit.service).
	unit string
	// revert are root script lines that undo the change.
	revert []string
	// armedAt is when the timer was armed.
	armedAt time.Time
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

// deadline is when the armed revert runs.
func (g *guard) deadline(r *Runner) time.Time { return g.armedAt.Add(r.revertAfter()) }

// arm starts the transient timer. A timer left over from an interrupted run
// is stopped first (its change is about to be rewritten).
func (r *Runner) arm(ctx context.Context, admin *transport.Remote, g *guard) error {
	secs := int(r.revertAfter() / time.Second)
	if secs < 1 {
		secs = 1
	}
	script := shell.Script(append([]string{"set +e"}, g.revert...)...)
	timer, service := shell.Quote(g.unit+".timer"), shell.Quote(g.unit+".service")
	lines := []string{
		"systemctl stop " + timer + " " + service + " >/dev/null 2>&1 || true",
		"systemctl reset-failed " + timer + " " + service + " >/dev/null 2>&1 || true",
		"systemd-run --quiet --collect --unit=" + shell.Quote(g.unit) + " --on-active=" + strconv.Itoa(secs) + "s /bin/sh -c " + shell.Quote(script),
	}
	r.printf("  armed %s.timer: it undoes this change in %ds unless new logins succeed first\n", g.unit, secs)
	g.armedAt = r.now()
	if err := r.runRoot(ctx, admin, lines); err != nil {
		return fmt.Errorf("%s: arming the automatic revert (systemd-run) failed, so nothing was changed: %w", g.name, err)
	}
	return nil
}

const (
	stepDisarm    = "revert-disarm"
	revertElapsed = "revert-elapsed"
)

// disarm stops the timer before it fires. elapsed reports that it had
// already fired (or is firing), so the change was undone.
func (r *Runner) disarm(ctx context.Context, admin *transport.Remote, g *guard, cleanup []string) (elapsed bool, err error) {
	timer := shell.Quote(g.unit + ".timer")
	steps := []*Step{
		{ID: stepDisarm, lines: append([]string{
			// A timer that is no longer waiting has fired (or is firing).
			`if test "$(systemctl show -p SubState --value ` + timer + ` 2>/dev/null)" != waiting; then echo ` + shell.Quote(doneMarker+revertElapsed) + `; echo "` + g.unit + `.timer is not waiting any more" >&2; exit 1; fi`,
			"systemctl stop " + timer,
		}, cleanup...)},
	}
	done, err := r.applySteps(ctx, admin, steps)
	if err == nil {
		return false, nil
	}
	if contains(done, revertElapsed) {
		return true, err
	}
	return false, err
}

// finish disarms g after the change was verified. A failed disarm is
// reported loudly: the timer would undo the verified change.
func (r *Runner) finish(ctx context.Context, admin *transport.Remote, g *guard, cleanup []string) error {
	elapsed, err := r.disarm(ctx, admin, g, cleanup)
	switch {
	case err == nil:
		r.printf("  verified; disarmed %s.timer\n", g.unit)
		return nil
	case elapsed:
		return fmt.Errorf("%s: the change was verified, but %s.timer had already run (the checks took longer than %s), so the change was undone; run host init again", g.name, g.unit, r.revertAfter())
	}
	r.printf("WARNING: %s: could not disarm %s.timer (%v); it will undo the change at %s\n", g.name, g.unit, err, g.deadline(r).Format("15:04:05"))
	return fmt.Errorf("%s: the change was verified, but disarming %s.timer failed (%v), so it will undo the change at %s; run host init again, or stop the timer yourself with `sudo systemctl stop %s.timer`", g.name, g.unit, err, g.deadline(r).Format("15:04:05 MST"), g.unit)
}

// rollback undoes g after a failure (cause): at once through the admin
// connection when that still works (then the timer is disarmed), otherwise
// by waiting for the armed timer and checking that a new admin login works
// again. The returned error says exactly what happened.
func (r *Runner) rollback(ctx context.Context, admin *transport.Remote, adminT transport.Target, g *guard, cause error) error {
	r.printf("  %v\n  rolling back...\n", cause)
	rerr := r.runRoot(ctx, admin, g.revert)
	if rerr == nil {
		if _, derr := r.disarm(ctx, admin, g, nil); derr != nil {
			return fmt.Errorf("%s: %v; rolled back the change, but disarming %s.timer failed (%v; it will run the same rollback again at %s, which is harmless)", g.name, cause, g.unit, derr, g.deadline(r).Format("15:04:05"))
		}
		return fmt.Errorf("%s: %v; rolled back the change and disarmed %s.timer", g.name, cause, g.unit)
	}
	at := g.deadline(r)
	wait := time.Until(at) + r.revertWait()
	if r.Now != nil {
		wait = at.Sub(r.now()) + r.revertWait()
	}
	r.printf("  the immediate rollback failed (%v)\n  waiting for %s.timer, armed on the host, to undo the change at %s...\n", rerr, g.unit, at.Format("15:04:05"))
	if wait > 0 {
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: %v; the immediate rollback failed (%v); %s.timer on the host undoes the change at %s (host init was interrupted before it could check)", g.name, cause, rerr, g.unit, at.Format("15:04:05 MST"))
		case <-time.After(wait):
		}
	}
	if verr := r.verifyLoginRetry(ctx, adminT); verr != nil {
		return fmt.Errorf("%s: %v; the immediate rollback failed (%v); %s.timer on the host was due to undo the change at %s, but a new %s login still fails (%v): use the provider console to check %s", g.name, cause, rerr, g.unit, at.Format("15:04:05 MST"), userOf(adminT), verr, g.unit)
	}
	return fmt.Errorf("%s: %v; the immediate rollback failed (%v), so %s.timer on the host undid the change at %s; a new %s login works again", g.name, cause, rerr, g.unit, at.Format("15:04:05 MST"), userOf(adminT))
}

func userOf(t transport.Target) string {
	u, _ := t.SplitHost()
	return u
}

// verifyBoth opens new admin and deploy connections.
func (r *Runner) verifyBoth(ctx context.Context, adminT, deployT transport.Target) error {
	if err := r.verifyLoginRetry(ctx, deployT); err != nil {
		return fmt.Errorf("a new login as %s failed (%v)", userOf(deployT), err)
	}
	if err := r.verifyLoginRetry(ctx, adminT); err != nil {
		return fmt.Errorf("a new login as %s failed (%v)", userOf(adminT), err)
	}
	return nil
}

// applyFirewall enables the firewall step s as a guarded change.
func (r *Runner) applyFirewall(ctx context.Context, admin *transport.Remote, adminT, deployT transport.Target, s *Step) error {
	r.printf("==> %s\n", s.Title)
	g := &guard{name: "--firewall", unit: firewallRevertUnit, revert: s.revert}
	if err := r.arm(ctx, admin, g); err != nil {
		return err
	}
	if _, err := r.applySteps(ctx, admin, []*Step{{ID: StepFirewall, lines: s.lines}}); err != nil {
		return r.rollback(ctx, admin, adminT, g, fmt.Errorf("configuring the firewall failed: %w", err))
	}
	r.printf("  firewall enabled; checking new connections...\n")
	if err := r.verifyBoth(ctx, adminT, deployT); err != nil {
		return r.rollback(ctx, admin, adminT, g, fmt.Errorf("%v after enabling the firewall", err))
	}
	return r.finish(ctx, admin, g, nil)
}

// hardenSSH writes the sshd drop-in as a guarded change: the revert is
// armed first, the drop-in is validated (sshd -t, sshd -T), a new deploy
// login is proven before sshd reloads, and new deploy and admin logins are
// proven after it. Any failure restores the previous drop-in (or removes
// it) and reloads sshd: at once when possible, else through the timer.
func (r *Runner) hardenSSH(ctx context.Context, admin *transport.Remote, adminT, deployT transport.Target, o *Options, p *Plan, sshdUnit string) error {
	r.printf("==> Harden sshd\n")
	dir, file := r.raw(sshdDropInD), r.raw(SSHDropIn)
	prev, tmp := file+".depper-prev", file+".depper-tmp"
	restore := `if test -f ` + shell.Quote(prev) + `; then mv -f ` + shell.Quote(prev) + ` ` + shell.Quote(file) + `; else rm -f ` + shell.Quote(file) + `; fi; rm -f ` + shell.Quote(tmp)
	reload := `systemctl reload ssh.service 2>/dev/null || systemctl reload sshd.service 2>/dev/null || true`
	if sshdUnit != "" {
		reload = `systemctl reload ` + shell.Quote(sshdUnit+".service") + ` || ` + reload
	}
	g := &guard{name: "--harden-ssh", unit: sshRevertUnit, revert: []string{restore, reload}}
	if err := r.arm(ctx, admin, g); err != nil {
		return err
	}
	write := []string{
		"set -eu",
		"umask 022",
		"mkdir -p " + shell.Quote(dir),
		// Keep the previous drop-in for the revert (a leftover backup from
		// an interrupted run is the older original and is kept).
		"if test -f " + shell.Quote(file) + " && ! test -e " + shell.Quote(prev) + "; then cp -p " + shell.Quote(file) + " " + shell.Quote(prev) + "; fi",
		"printf '%s' " + shell.Quote(dropInContent(p.hardenRoot)) + " > " + shell.Quote(tmp),
		"chmod 644 " + shell.Quote(tmp),
		"mv -f " + shell.Quote(tmp) + " " + shell.Quote(file),
		`if ! depper_out=$(sshd -t 2>&1); then ` + restore + `; printf 'sshd -t rejected the configuration (drop-in removed): %s\n' "$depper_out" >&2; exit 1; fi`,
		`depper_bad=$(sshd -T 2>/dev/null | awk -v before=` + shell.Quote(p.rootBefore) + ` 'function rank(v) { if (v == "yes") return 0; if (v == "prohibit-password" || v == "without-password") return 1; if (v == "forced-commands-only") return 2; if (v == "no") return 3; return -1 } BEGIN { need = rank(before); if (need < 1) need = 1 } ($1 == "passwordauthentication" && $2 != "no") || ($1 == "kbdinteractiveauthentication" && $2 != "no") || ($1 == "permitrootlogin" && rank($2) < need) { printf "%s %s; ", $1, $2 }')`,
		`if test -n "$depper_bad"; then ` + restore + `; printf 'the drop-in does not take effect (sshd -T: %s): an earlier configuration line sets these first; drop-in removed\n' "$depper_bad" >&2; exit 1; fi`,
	}
	if err := r.runRoot(ctx, admin, write); err != nil {
		if _, derr := r.disarm(ctx, admin, g, nil); derr != nil {
			return fmt.Errorf("--harden-ssh: %w; disarming %s.timer also failed (%v; it only restores the previous drop-in and reloads sshd)", err, g.unit, derr)
		}
		return fmt.Errorf("--harden-ssh: %w", err)
	}
	r.printf("  checking that %s can log in with its key before reloading sshd...\n", o.User)
	if err := r.verifyLogin(ctx, deployT); err != nil {
		if rerr := r.runRoot(ctx, admin, []string{restore}); rerr != nil {
			return fmt.Errorf("--harden-ssh: %s cannot log in with its key (%v), and removing %s failed: %v; %s.timer removes it at %s", o.User, err, SSHDropIn, rerr, g.unit, g.deadline(r).Format("15:04:05"))
		}
		_, _ = r.disarm(ctx, admin, g, nil)
		return fmt.Errorf("--harden-ssh: %s cannot log in with its key over a new connection (%v); removed %s and did not reload sshd", o.User, err, SSHDropIn)
	}
	if err := r.runRoot(ctx, admin, []string{
		`if systemctl is-active --quiet ssh.service; then depper_unit=ssh.service; elif systemctl is-active --quiet sshd.service; then depper_unit=sshd.service; else echo "cannot find the running sshd service (ssh.service or sshd.service)" >&2; exit 1; fi`,
		`systemctl reload "$depper_unit"`,
	}); err != nil {
		return r.rollback(ctx, admin, adminT, g, fmt.Errorf("reloading sshd failed: %w", err))
	}
	r.printf("  sshd reloaded; checking new connections...\n")
	if err := r.verifyBoth(ctx, adminT, deployT); err != nil {
		return r.rollback(ctx, admin, adminT, g, fmt.Errorf("%v after reloading sshd", err))
	}
	return r.finish(ctx, admin, g, []string{"rm -f " + shell.Quote(prev)})
}
