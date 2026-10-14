package host

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const sshGuardDir = "fs" + guardStateDir + "/" + sshRevertUnit

// waitEvent waits until timer-events contains want.
func (h *fakeHost) waitEvent(want string, limit time.Duration) {
	h.t.Helper()
	end := time.Now().Add(limit)
	for !strings.Contains(h.read("timer-events"), want) {
		if time.Now().After(end) {
			h.t.Fatalf("no %q in timer events within %s: %q", want, limit, h.read("timer-events"))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (h *fakeHost) noGuardMarkers(unit string) {
	h.t.Helper()
	for _, m := range append(append([]string{}, guardMarkers...), markPrev, markReloaded) {
		if h.has("fs" + guardStateDir + "/" + unit + "/" + m) {
			h.t.Errorf("%s marker %s left behind", unit, m)
		}
	}
}

// An earlier drop-in is restored byte for byte from the had-prev marker,
// and running the revert again changes nothing.
func TestRevertRestoresAnExistingDropIn(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, debian12)
	const old = "# written by hand\nPasswordAuthentication no\n"
	h.write("fs/etc/ssh/sshd_config.d/10-depper.conf", old, 0o600)
	h.write("deny-deploy-after-reload", "", 0o644)
	o := h.options()
	o.HardenSSH = true
	_, err := h.runner().Init(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "rolled back the change and disarmed depper-ssh-revert.timer") {
		t.Fatalf("err = %v", err)
	}
	if got := h.read("fs/etc/ssh/sshd_config.d/10-depper.conf"); got != old {
		t.Errorf("drop-in after the rollback = %q, want %q", got, old)
	}
	h.noGuardMarkers(sshRevertUnit)
	// Without markers the revert script is inert (it is removed on
	// disarm; rebuild it from the guard to run it once more).
	r := h.runner()
	script := r.revertScriptBody(r.sshGuard("ssh"))
	cmd := exec.Command("/bin/sh", "-c", script, "revert", "now")
	cmd.Env = append(cmd.Environ(), "PATH="+fakeRemotePATH(h.Root), "FAKE_ROOT="+h.Root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("second revert: %v\n%s", err, out)
	}
	if got := h.read("fs/etc/ssh/sshd_config.d/10-depper.conf"); got != old {
		t.Errorf("drop-in after a second revert = %q", got)
	}
	if got := strings.Count(h.read("sshd-reloads"), "reload"); got != 2 {
		t.Errorf("reloads = %d, want 2 (apply + rollback; the inert revert does not reload)", got)
	}
}

// The timer elapses while host init rolls back on its own: both run the
// same revert, the earlier drop-in comes back once, and the error says the
// timer ran too.
func TestRevertTimerAndImmediateRollbackBothRun(t *testing.T) {
	t.Parallel()
	t.Run("timer first", func(t *testing.T) {
		t.Parallel()
		h := newFakeHost(t, debian12)
		const old = "PasswordAuthentication no\n"
		h.write("fs/etc/ssh/sshd_config.d/10-depper.conf", old, 0o644)
		h.write("timer-fires-on-reload", "", 0o644)
		h.write("deny-deploy-after-reload", "", 0o644)
		o := h.options()
		o.HardenSSH = true
		_, err := h.runner().Init(context.Background(), o)
		if err == nil || !strings.Contains(err.Error(), "and depper-ssh-revert had also started the same revert (the two ran one after the other; the second had nothing left to undo)") {
			t.Fatalf("err = %v", err)
		}
		if got := h.read("fs/etc/ssh/sshd_config.d/10-depper.conf"); got != old {
			t.Errorf("drop-in = %q, want %q", got, old)
		}
		h.noGuardMarkers(sshRevertUnit)
	})
	t.Run("rollback first", func(t *testing.T) {
		t.Parallel()
		h := newFakeHost(t, debian12)
		const old = "PasswordAuthentication no\n"
		h.write("fs/etc/ssh/sshd_config.d/10-depper.conf", old, 0o644)
		h.write("timer-stop-fails", "", 0o644)
		h.write("deny-deploy-after-reload", "", 0o644)
		o := h.options()
		o.HardenSSH = true
		r := h.runner()
		r.revertDelay = 15 * time.Second
		_, err := r.Init(context.Background(), o)
		if err == nil || !strings.Contains(err.Error(), "rolled back the change, but stopping depper-ssh-revert.timer failed") || !strings.Contains(err.Error(), "which then has nothing left to undo") {
			t.Fatalf("err = %v", err)
		}
		h.waitEvent("done depper-ssh-revert\n", 40*time.Second)
		if got := h.read("fs/etc/ssh/sshd_config.d/10-depper.conf"); got != old {
			t.Errorf("drop-in after the timer also ran = %q, want %q", got, old)
		}
		if got := strings.Count(h.read("sshd-reloads"), "reload"); got != 2 {
			t.Errorf("reloads = %d, want 2 (apply + rollback; the late timer had nothing to undo)", got)
		}
	})
}

// A run killed after writing the drop-in leaves the timer armed. The next
// run finishes that revert first, says so, and then hardens normally.
func TestRerunFinishesAnInterruptedHardening(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, debian12)
	o := h.options()
	o.HardenSSH = true
	r := h.runner()
	r.interruptAt = StepHardenSSH
	if _, err := r.Init(context.Background(), o); !errors.Is(err, errInterrupted) {
		t.Fatalf("err = %v", err)
	}
	if !h.has("units/depper-ssh-revert.timer.active") || !h.has(sshGuardDir+"/had-none") || !h.has("fs/etc/ssh/sshd_config.d/10-depper.conf") {
		t.Fatal("the interrupted run did not leave an armed revert")
	}
	// A dry run only reports it.
	dry := h.optionsWith(func(o *Options) { o.HardenSSH, o.DryRun = true, true })
	res, err := h.runner().Init(context.Background(), dry)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "an interrupted host init left an unverified change pending (depper-ssh-revert.timer waiting, revert markers present)") {
		t.Errorf("dry-run warnings = %v", res.Warnings)
	}
	if strings.Contains(h.read("timer-events"), "started") {
		t.Error("the dry run ran the revert")
	}
	res = mustInit(t, h, h.optionsWith(func(o *Options) { o.HardenSSH = true }))
	out := h.Out.String()
	if !strings.Contains(out, "Finishing an interrupted host init: its --harden-ssh change was never verified, so depper-ssh-revert reverts it now (timer waiting).") {
		t.Errorf("output:\n%s", out)
	}
	ev := h.read("timer-events")
	start, rearm := strings.Index(ev, "started depper-ssh-revert.service\ndone depper-ssh-revert\n"), strings.LastIndex(ev, "armed depper-ssh-revert\n")
	if start < 0 || rearm < start || strings.Count(ev, "armed depper-ssh-revert\n") != 2 {
		t.Errorf("timer events = %q", ev)
	}
	if !contains(res.Applied, StepHardenSSH) || h.read("fs/etc/ssh/sshd_config.d/10-depper.conf") != dropInContent(true) {
		t.Errorf("applied %v, drop-in %q", res.Applied, h.read("fs/etc/ssh/sshd_config.d/10-depper.conf"))
	}
	h.noGuardMarkers(sshRevertUnit)
	if h.has("units/depper-ssh-revert.timer.active") {
		t.Error("timer still armed")
	}
}

// The timer fires later than host init waits: host init reports that the
// login still fails, the revert still happens, and the next run starts
// clean (the elapsed timer is cleared, not treated as pending).
func TestLateFiringRevertTimer(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, debian12)
	h.write("timer-delay", "25", 0o644)
	h.write("deny-admin-after-reload", "", 0o644)
	o := h.options()
	o.HardenSSH = true
	r := h.runner()
	r.revertDelay, r.revertGrace = 8*time.Second, 2*time.Second
	_, err := r.Init(context.Background(), o)
	if err == nil {
		t.Fatal("hardening succeeded although the admin cannot log in")
	}
	for _, want := range []string{"the immediate rollback failed", "depper-ssh-revert.timer on the host was due to undo the change at", "a new root login still fails", "use the provider console"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if strings.Contains(h.read("timer-events"), "fired") {
		t.Fatal("the timer fired before host init gave up; the test does not cover a late timer")
	}
	h.waitEvent("done depper-ssh-revert\n", 40*time.Second)
	if h.has("fs/etc/ssh/sshd_config.d/10-depper.conf") {
		t.Error("the late timer did not remove the drop-in")
	}
	h.noGuardMarkers(sshRevertUnit)
	h.remove("deny-admin-after-reload")
	h.remove("timer-delay")
	before := len(h.Out.String())
	mustInit(t, h, h.optionsWith(func(o *Options) { o.HardenSSH = true }))
	out := h.Out.String()[before:]
	if strings.Contains(out, "Finishing an interrupted host init") || !strings.Contains(out, "clearing the finished depper-ssh-revert.timer of an earlier run") {
		t.Errorf("output:\n%s", out)
	}
}

// systemd-run failing after the markers were written: the markers are
// removed again and the error says exactly that.
func TestArmReportsWhatChangedWhenSystemdRunFails(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, debian12)
	h.write("systemd-run-fails", "", 0o644)
	o := h.options()
	o.HardenSSH = true
	_, err := h.runner().Init(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "systemd-run failed (Failed to start transient timer unit: Access denied), so host init stopped before changing anything else: it had written the revert script and markers to") || !strings.Contains(err.Error(), "and removed them again") {
		t.Fatalf("err = %v", err)
	}
	h.noGuardMarkers(sshRevertUnit)
	if h.has(sshGuardDir+"/"+revertScript) || h.has("fs/etc/ssh/sshd_config.d/10-depper.conf") || h.has("sshd-reloads") {
		t.Error("the host changed although arming failed")
	}
}

// A PermitRootLogin line an earlier build wrote over a stricter setting is
// dropped again: the value is read without depper's drop-in.
func TestHardenSSHDropsAWeakerRootLoginFromEarlierBuilds(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, debian12)
	h.write("fs/etc/ssh/sshd_config", "Include /etc/ssh/sshd_config.d/*.conf\nPermitRootLogin no\nPasswordAuthentication yes\n", 0o644)
	h.write("fs/etc/ssh/sshd_config.d/10-depper.conf", dropInContent(true), 0o644)
	o := h.options()
	o.HardenSSH = true
	res := mustInit(t, h, o)
	if got := h.read("fs/etc/ssh/sshd_config.d/10-depper.conf"); got != dropInContent(false) {
		t.Errorf("drop-in = %q", got)
	}
	if res.Facts.SSHDRootBase != "no" || !contains(res.Applied, StepHardenSSH) {
		t.Errorf("root base %q, applied %v", res.Facts.SSHDRootBase, res.Applied)
	}
	// With yes underneath, the line is kept and nothing changes.
	h2 := newFakeHost(t, debian12)
	h2.write("fs/etc/ssh/sshd_config.d/10-depper.conf", dropInContent(true), 0o644)
	res = mustInit(t, h2, h2.optionsWith(func(o *Options) { o.HardenSSH = true }))
	if ids := planChanges(res); contains(ids, StepHardenSSH) || res.Facts.SSHDRootBase != "yes" {
		t.Errorf("changes %v, root base %q", ids, res.Facts.SSHDRootBase)
	}
}

// The firewall also allows every ListenAddress port and the ports of an
// active ssh.socket.
func TestFirewallAllowsListenAddressAndSocketPorts(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, debian12)
	h.write("fs/etc/ssh/sshd_config", "Include /etc/ssh/sshd_config.d/*.conf\nListenAddress 0.0.0.0:2200\nListenAddress [::]:2201\nPermitRootLogin yes\n", 0o644)
	h.write("units/ssh.socket.active", "", 0o644)
	h.write("ssh.socket.listen", "[::]:2022 (Stream)\n0.0.0.0:2022 (Stream)\n", 0o644)
	o := h.options()
	o.Firewall = []int{80}
	mustInit(t, h, o)
	log := h.read("ufw.log")
	for _, p := range []string{"22", "80", "2022", "2200", "2201"} {
		if !strings.Contains(log, "ufw allow "+p+"/tcp\n") {
			t.Errorf("port %s not allowed:\n%s", p, log)
		}
	}
}

func TestListenPorts(t *testing.T) {
	for in, want := range map[string]string{
		"[::]:22 (Stream) 0.0.0.0:2222 (Stream)": "22,2222",
		"22 (Stream)":                            "22",
		"0.0.0.0:53 (Datagram) [::]:22 (Stream)": "22",
		"":                                       "",
		"/run/x.sock (Stream)":                   "",
	} {
		if got := intsString(listenPorts(in)); got != want {
			t.Errorf("listenPorts(%q) = %q, want %q", in, got, want)
		}
	}
}

// Guarded steps need systemd-run with --collect (systemd 236+), but only
// when they change something; Amazon Linux 2 is refused.
func TestPlatformGating(t *testing.T) {
	t.Parallel()
	t.Run("old systemd", func(t *testing.T) {
		t.Parallel()
		h := newFakeHost(t, debian12)
		h.write("systemd-version", "235", 0o644)
		_, err := h.runner().Init(context.Background(), h.optionsWith(func(o *Options) { o.HardenSSH = true }))
		if err == nil || !strings.Contains(err.Error(), "--harden-ssh: the host runs systemd 235, whose systemd-run has no --collect; depper needs systemd 236 or later") {
			t.Fatalf("err = %v", err)
		}
		if h.changes() != "" {
			t.Errorf("changed the host:\n%s", h.changes())
		}
	})
	t.Run("no systemd-run, nothing to guard", func(t *testing.T) {
		t.Parallel()
		h := newFakeHost(t, debian12)
		o := h.optionsWith(func(o *Options) { o.HardenSSH, o.Firewall = true, []int{80} })
		mustInit(t, h, o)
		h.remove("remote-bin/systemd-run")
		h.write("systemd-version", "219", 0o644)
		res := mustInit(t, h, h.optionsWith(func(o *Options) { o.HardenSSH, o.Firewall = true, []int{80} }))
		if ids := planChanges(res); contains(ids, StepHardenSSH) || contains(ids, StepFirewall) {
			t.Errorf("changes %v", ids)
		}
		_, err := h.runner().Init(context.Background(), h.optionsWith(func(o *Options) { o.Firewall = []int{80, 443} }))
		if err == nil || !strings.Contains(err.Error(), "--firewall: systemd-run was not found") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("amazon linux 2", func(t *testing.T) {
		t.Parallel()
		h := newFakeHost(t, distro{
			osRelease: "NAME=\"Amazon Linux\"\nVERSION_ID=\"2\"\nID=\"amzn\"\nID_LIKE=\"centos rhel fedora\"\nPRETTY_NAME=\"Amazon Linux 2\"\n",
			sshUnit:   "sshd.service",
			installed: []string{"coreutils", "ca-certificates", "shadow-utils", "curl"},
			preBin:    []string{"newuidmap", "curl"},
		})
		_, err := h.runner().Init(context.Background(), h.optionsWith(func(o *Options) { o.Container = ContainerDocker }))
		var unsupported *ErrUnsupported
		if !errors.As(err, &unsupported) || !strings.Contains(err.Error(), "Amazon Linux 2 is not supported") || !strings.Contains(err.Error(), "2023") {
			t.Fatalf("err = %v", err)
		}
		if h.changes() != "" {
			t.Errorf("changed the host:\n%s", h.changes())
		}
	})
}

// runRevert runs a guard's revert script on the fake host as WHO and
// returns its exit error and output.
func (h *fakeHost) runRevert(unit, who string) (string, error) {
	h.t.Helper()
	cmd := exec.Command("/bin/sh", h.FS+guardStateDir+"/"+unit+"/"+revertScript, who)
	cmd.Env = append(cmd.Environ(), "PATH="+fakeRemotePATH(h.Root), "FAKE_ROOT="+h.Root)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// installRevert writes g's revert script into the fake host's guard
// directory, as arm does.
func (h *fakeHost) installRevert(r *Runner, g *guard) {
	h.t.Helper()
	h.write("fs"+guardStateDir+"/"+g.unit+"/"+revertScript, r.revertScriptBody(g), 0o644)
}

// A revert step that fails keeps its marker, writes no reverted marker and
// exits 1; the next attempt finishes the job.
func TestRevertFailureKeepsItsMarkers(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, debian12)
	r := h.runner()
	h.installRevert(r, r.sshGuard("ssh"))
	const old = "PasswordAuthentication no\n"
	h.write("fs/etc/ssh/sshd_config.d/10-depper.conf", dropInContent(true), 0o644)
	h.write(sshGuardDir+"/had-prev", "", 0o644)
	// prev is a directory: cp fails.
	h.write(sshGuardDir+"/prev/x", "", 0o644)
	out, err := h.runRevert(sshRevertUnit, "timer")
	if err == nil || !strings.Contains(out, "could not restore") || !strings.Contains(out, "the revert did not complete") {
		t.Fatalf("revert: err %v, output %q", err, out)
	}
	if !h.has(sshGuardDir+"/had-prev") || h.has(sshGuardDir+"/reverted") || h.read("fs/etc/ssh/sshd_config.d/10-depper.conf") != dropInContent(true) {
		t.Error("a failed restore removed its marker, wrote reverted, or changed the drop-in")
	}
	h.remove(sshGuardDir + "/prev")
	h.write(sshGuardDir+"/prev", old, 0o644)
	if out, err := h.runRevert(sshRevertUnit, "pending"); err != nil {
		t.Fatalf("second revert: %v\n%s", err, out)
	}
	if h.read("fs/etc/ssh/sshd_config.d/10-depper.conf") != old || !strings.HasPrefix(h.read(sshGuardDir+"/reverted"), "pending ") {
		t.Errorf("drop-in %q, reverted %q", h.read("fs/etc/ssh/sshd_config.d/10-depper.conf"), h.read(sshGuardDir+"/reverted"))
	}
	h.noGuardMarkers(sshRevertUnit)

	// A firewall port that cannot be closed keeps added-ports.
	fp, fr := firewallMarkers(firewallGuard{added: []int{80}, remove: func(string) string { return "false" }})
	g := &guard{name: "--firewall", unit: firewallRevertUnit, prepare: fp, revert: fr}
	h.installRevert(r, g)
	h.write("fs"+guardStateDir+"/"+firewallRevertUnit+"/added-ports", "80\n", 0o644)
	if out, err := h.runRevert(firewallRevertUnit, "now"); err == nil || !strings.Contains(out, "could not close port 80/tcp again") {
		t.Errorf("firewall revert: err %v, output %q", err, out)
	}
	if !h.has("fs" + guardStateDir + "/" + firewallRevertUnit + "/added-ports") {
		t.Error("added-ports removed although the port is still open")
	}
}

// The revert waits for the guard lock (held by disarm or another revert),
// and a revert the timer starts after the guard was disarmed does nothing.
func TestRevertHoldsTheGuardLock(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, debian12)
	r := h.runner()
	h.installRevert(r, r.sshGuard("ssh"))
	h.write("fs/etc/ssh/sshd_config.d/10-depper.conf", dropInContent(true), 0o644)
	h.write(sshGuardDir+"/had-none", "", 0o644)
	h.write(sshGuardDir+"/lock/.keep", "", 0o644)
	h.remove(sshGuardDir + "/lock/.keep")
	cmd := exec.Command("/bin/sh", h.FS+guardStateDir+"/"+sshRevertUnit+"/"+revertScript, "timer")
	cmd.Env = append(cmd.Environ(), "PATH="+fakeRemotePATH(h.Root), "FAKE_ROOT="+h.Root)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	if !h.has("fs/etc/ssh/sshd_config.d/10-depper.conf") || h.has(sshGuardDir+"/started") {
		t.Fatal("the revert ran while the lock was held")
	}
	// The holder disarmed the guard meanwhile (as disarm does under the
	// lock), then releases it.
	h.write(sshGuardDir+"/disarmed", "", 0o644)
	h.remove(sshGuardDir + "/had-none")
	h.remove(sshGuardDir + "/lock")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("revert: %v", err)
	}
	if !h.has("fs/etc/ssh/sshd_config.d/10-depper.conf") || h.has(sshGuardDir+"/started") || h.has(sshGuardDir+"/reverted") || h.has("sshd-reloads") {
		t.Error("a revert started after disarming changed something")
	}
}

// Transient timers do not survive a reboot: the boot service runs the
// revert of a change a reboot left unverified, and when it did not run,
// the next host init finishes it from the markers.
func TestRevertAfterAReboot(t *testing.T) {
	t.Parallel()
	interrupted := func(t *testing.T) *fakeHost {
		h := newFakeHost(t, debian12)
		r := h.runner()
		r.interruptAt = StepHardenSSH
		if _, err := r.Init(context.Background(), h.optionsWith(func(o *Options) { o.HardenSSH = true })); !errors.Is(err, errInterrupted) {
			t.Fatalf("err = %v", err)
		}
		// The reboot: the transient timer is gone.
		h.remove("units/depper-ssh-revert.timer.active")
		h.waitEvent("cancelled depper-ssh-revert\n", 5*time.Second)
		return h
	}
	t.Run("boot service", func(t *testing.T) {
		t.Parallel()
		h := interrupted(t)
		if !h.has("units/"+bootUnit+".enabled") || !strings.Contains(h.read("fs/etc/systemd/system/"+bootUnit), "ExecStart=/bin/sh "+h.FS+guardStateDir+"/"+bootScript) {
			t.Fatalf("boot unit not installed and enabled: %q", h.read("fs/etc/systemd/system/"+bootUnit))
		}
		cmd := exec.Command("/bin/sh", h.FS+guardStateDir+"/"+bootScript)
		cmd.Env = append(cmd.Environ(), "PATH="+fakeRemotePATH(h.Root), "FAKE_ROOT="+h.Root)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("boot script: %v\n%s", err, out)
		}
		if h.has("fs/etc/ssh/sshd_config.d/10-depper.conf") || !strings.HasPrefix(h.read(sshGuardDir+"/reverted"), "boot ") || !h.has("sshd-reloads") {
			t.Error("the boot revert did not remove the drop-in and reload sshd")
		}
		h.noGuardMarkers(sshRevertUnit)
		before := len(h.Out.String())
		mustInit(t, h, h.optionsWith(func(o *Options) { o.HardenSSH = true }))
		if strings.Contains(h.Out.String()[before:], "Finishing an interrupted host init") {
			t.Error("a reverted guard was treated as pending")
		}
	})
	t.Run("markers only", func(t *testing.T) {
		t.Parallel()
		h := interrupted(t)
		before := len(h.Out.String())
		res := mustInit(t, h, h.optionsWith(func(o *Options) { o.HardenSSH = true }))
		if !strings.Contains(h.Out.String()[before:], "Finishing an interrupted host init: its --harden-ssh change was never verified, so depper-ssh-revert reverts it now (timer dead).") {
			t.Errorf("output:\n%s", h.Out.String()[before:])
		}
		if strings.Contains(h.read("timer-events"), "started depper-ssh-revert.service") || !contains(res.Applied, StepHardenSSH) {
			t.Errorf("timer events %q, applied %v", h.read("timer-events"), res.Applied)
		}
		h.noGuardMarkers(sshRevertUnit)
	})
}

// An interrupted firewalld change is reverted in the zone recorded when
// it was armed, even if the interface's zone changed since.
func TestInterruptedFirewalldRevertUsesTheRecordedZone(t *testing.T) {
	t.Parallel()
	h := newFakeHost(t, rocky9)
	h.write("packages", "coreutils\nca-certificates\nshadow-utils\ncurl\nfirewalld\n", 0o644)
	h.write("pkg-bin/firewall-cmd", fakeFirewallCmd, 0o755)
	h.write("pkg-bin/firewall-offline-cmd", fakeFirewallCmd, 0o755)
	h.write("units/firewalld.service.active", "", 0o644)
	h.write("firewalld-iface-zones", "eth0 internal\n", 0o644)
	r := h.runner()
	r.interruptAt = StepFirewall
	if _, err := r.Init(context.Background(), h.optionsWith(func(o *Options) { o.Firewall = []int{443} })); !errors.Is(err, errInterrupted) {
		t.Fatalf("err = %v", err)
	}
	if h.read("fwd/internal.runtime") != "443/tcp\n" || h.read("fs"+guardStateDir+"/"+firewallRevertUnit+"/zone") != "internal\n" {
		t.Fatalf("runtime %q zone %q", h.read("fwd/internal.runtime"), h.read("fs"+guardStateDir+"/"+firewallRevertUnit+"/zone"))
	}
	h.write("firewalld-iface-zones", "eth0 dmz\n", 0o644)
	res, err := h.runner().Init(context.Background(), h.optionsWith(func(o *Options) { o.DryRun = true }))
	if err != nil || !strings.Contains(strings.Join(res.Warnings, "\n"), "depper-firewall-revert.timer waiting") {
		t.Fatalf("dry run: %v %v", err, res.Warnings)
	}
	mustInit(t, h, h.options())
	if !strings.Contains(h.Out.String(), "Finishing an interrupted host init: its --firewall change was never verified") {
		t.Errorf("output:\n%s", h.Out.String())
	}
	if h.read("fwd/internal.runtime") != "" || h.read("fwd/internal.perm") != "" {
		t.Errorf("runtime %q perm %q after the revert", h.read("fwd/internal.runtime"), h.read("fwd/internal.perm"))
	}
	h.noGuardMarkers(firewallRevertUnit)
}
