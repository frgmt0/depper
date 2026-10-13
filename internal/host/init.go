package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/frgmt0/depper/internal/doctor"
	"github.com/frgmt0/depper/internal/shell"
	"github.com/frgmt0/depper/internal/transport"
)

// KeyInfo is a host key as shown to the user.
type KeyInfo struct {
	Type        string `json:"type"`
	Fingerprint string `json:"fingerprint"`
}

// InitResult is the outcome of host init; it is also the --json document.
type InitResult struct {
	Host          string         `json:"host"`
	Port          int            `json:"port"`
	Admin         string         `json:"admin"`
	User          string         `json:"user"`
	Target        string         `json:"target"`
	DryRun        bool           `json:"dry_run"`
	KnownHosts    string         `json:"known_hosts"`
	HostKeys      []KeyInfo      `json:"host_keys"`
	HostKeyStatus string         `json:"host_key_status"`
	Facts         *Facts         `json:"facts"`
	Plan          []*Step        `json:"plan"`
	Applied       []string       `json:"applied"`
	Warnings      []string       `json:"warnings"`
	Doctor        *doctor.Report `json:"doctor"`
	ConfigFile    string         `json:"config_file"`
	ConfigChanges []ConfigChange `json:"config_changes"`
	ConfigWritten bool           `json:"config_written"`
	Next          string         `json:"next"`
	Error         string         `json:"error,omitempty"`
}

// Host key states reported in InitResult.HostKeyStatus.
const (
	HostKeyPinned      = "already-pinned"
	HostKeyAdded       = "added"
	HostKeyUnconfirmed = "unconfirmed-dry-run"
)

// ErrDeclined reports that the user answered no.
var ErrDeclined = errors.New("cancelled; nothing was changed on the host")

func (r *Runner) confirm(o *Options, question string) error {
	if o.Yes {
		return nil
	}
	if r.Confirm == nil {
		return fmt.Errorf("confirmation required: re-run with --yes (stdin is not a terminal)")
	}
	ok, err := r.Confirm(question)
	if err != nil {
		return err
	}
	if !ok {
		return ErrDeclined
	}
	return nil
}

func (r *Runner) remote(t transport.Target) *transport.Remote {
	rem := transport.NewRemote(r.Exec, t)
	rem.SSHProgram = r.SSHProgram
	rem.Stdout, rem.Stderr = r.out(), r.out()
	return rem
}

// Init runs `depper host init`. The result is returned even on failure, with
// as much filled in as was reached.
func (r *Runner) Init(ctx context.Context, o *Options) (*InitResult, error) {
	res := &InitResult{Host: o.Host, Port: o.Port, Admin: o.Admin, User: o.User, Target: o.Target, DryRun: o.DryRun,
		KnownHosts: o.KnownHosts, HostKeys: []KeyInfo{}, Plan: []*Step{}, Applied: []string{}, Warnings: []string{}, ConfigChanges: []ConfigChange{}}
	err := r.init(ctx, o, res)
	if err != nil {
		res.Error = err.Error()
	}
	return res, err
}

func (r *Runner) init(ctx context.Context, o *Options, res *InitResult) error {
	if err := o.Validate(); err != nil {
		return err
	}
	res.Port = o.Port
	key, err := ReadPublicKey(o.KeyFile)
	if err != nil {
		return err
	}
	deployIdentity := PrivateKeyFor(o.KeyFile)
	configIdentity := ""
	if o.KeyExplicit {
		configIdentity = deployIdentity
	}

	// The configuration must exist and accept the edit before the host is
	// touched.
	edit, cfg, err := planConfig(o.Repo, o.Target, o, configIdentity, r.LookupEnv)
	if err != nil {
		return err
	}
	res.ConfigFile, res.ConfigChanges = edit.File, edit.Changes
	if o.Container == ContainerAuto {
		o.Container = ContainerNone
		if cfg.Container != nil {
			o.Container = cfg.Container.Engine
		}
		if err := o.Validate(); err != nil {
			return err
		}
	}

	// 1. Host key.
	name := KnownHostsName(o.Host, o.Port)
	scanned, err := r.keyscan(ctx, o.Host, o.Port)
	if err != nil {
		return err
	}
	for _, k := range scanned {
		res.HostKeys = append(res.HostKeys, KeyInfo{Type: k.Type, Fingerprint: k.Fingerprint()})
	}
	existing, err := readKnownHosts(o.KnownHosts)
	if err != nil {
		return err
	}
	recorded, err := knownKeysFor(existing, name)
	if err != nil {
		return err
	}
	state, err := comparePins(o.KnownHosts, name, recorded, scanned)
	if err != nil {
		return err
	}
	knownHosts := o.KnownHosts
	if state == pinPinned {
		res.HostKeyStatus = HostKeyPinned
		r.printf("Host key for %s is already pinned in %s:\n", name, o.KnownHosts)
		r.printKeys(scanned)
	} else {
		r.printf("The host %s presents these SSH host keys:\n", name)
		r.printKeys(scanned)
		r.printf("Compare them with the fingerprints from your provider's console (or `ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub` on the host).\n")
		if o.DryRun {
			res.HostKeyStatus = HostKeyUnconfirmed
			r.printf("Dry run: the keys are not confirmed or written; facts are read over a connection that trusts them for this run only.\n")
			tmp, err := os.MkdirTemp("", "depper-known-hosts-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(tmp)
			knownHosts = filepath.Join(tmp, "known_hosts")
			if err := writeKnownHosts(knownHosts, "", name, scanned); err != nil {
				return err
			}
		} else {
			if err := r.confirm(o, "Trust these host keys and write them to "+o.KnownHosts+"?"); err != nil {
				return err
			}
			if err := writeKnownHosts(o.KnownHosts, existing, name, scanned); err != nil {
				return err
			}
			res.HostKeyStatus = HostKeyAdded
			res.Applied = append(res.Applied, StepKnownHosts)
			r.printf("Pinned the host keys in %s.\n", o.KnownHosts)
		}
	}

	// 2. Facts, as the admin.
	adminTarget := transport.Target{Host: o.Admin + "@" + o.Host, Port: o.Port, IdentityFile: o.Identity, KnownHosts: knownHosts}
	admin := r.remote(adminTarget)
	facts, err := r.gatherFacts(ctx, admin, o, key)
	if err != nil {
		return err
	}
	res.Facts = facts

	// 3. The plan.
	plan, err := r.buildPlan(o, facts, key)
	if err != nil {
		return err
	}
	khStep := &Step{ID: StepKnownHosts, Title: "Pin the host key in " + displayPath(o.KnownHosts, cfg.Repo)}
	switch res.HostKeyStatus {
	case HostKeyPinned:
		khStep.Detail = "already pinned"
	case HostKeyAdded:
		khStep.Detail = "written after confirmation"
	default:
		khStep.Change, khStep.Detail = true, "write "+fmt.Sprint(len(scanned))+" key(s) for "+name+" (mode 600)"
	}
	cfgStep := &Step{ID: StepConfig, Title: "Write the connection to " + filepath.Base(edit.File) + " [" + edit.Table + "]"}
	if !o.WriteConfig {
		cfgStep.Detail = "skipped (--no-write-config)"
	} else if edit.Changed() {
		cfgStep.Change = true
		var parts []string
		for _, c := range edit.Changes {
			if c.Changed {
				parts = append(parts, c.String())
			}
		}
		cfgStep.Detail = strings.Join(parts, "; ")
	} else {
		cfgStep.Detail = "already up to date"
	}
	plan.Steps = append(append([]*Step{khStep}, plan.Steps...), cfgStep)
	if cfg.Container != nil && o.Container == ContainerNone {
		plan.warn("depper.toml has [container] (engine %s) but --container none installs no container engine; drop --container to install %s", cfg.Container.Engine, cfg.Container.Engine)
	}
	res.Plan, res.Warnings = plan.Steps, plan.Warnings
	r.printPlan(o, facts, plan)

	if o.DryRun {
		r.printf("Dry run: nothing was changed.\n")
		return nil
	}
	// The host key was confirmed on its own; everything else (remote steps
	// and the depper.toml edit) is confirmed here.
	pending := false
	for _, s := range plan.Steps {
		if s.Change && s.ID != StepKnownHosts {
			pending = true
		}
	}
	if pending {
		if err := r.confirm(o, "Apply these changes to "+o.Host+"?"); err != nil {
			return err
		}
	}

	// 4. Apply.
	var main []*Step
	for _, s := range plan.Steps {
		switch s.ID {
		case StepKnownHosts, StepConfig, StepHardenSSH, StepRecord:
			continue
		}
		if s.Change {
			main = append(main, s)
		}
	}
	appliedPkgs := []string{}
	var applyErr error
	if len(main) > 0 {
		done, err := r.applySteps(ctx, admin, main)
		res.Applied = append(res.Applied, done...)
		for _, s := range main {
			if contains(done, s.ID) {
				appliedPkgs = append(appliedPkgs, s.packages...)
			}
		}
		if err != nil {
			applyErr = fmt.Errorf("applying the plan failed after %d of %d step(s): %w", len(done), len(main), err)
		}
	}
	deployTarget := transport.Target{Host: o.User + "@" + o.Host, Port: o.Port, IdentityFile: deployIdentity, KnownHosts: o.KnownHosts}
	if s := plan.step(StepHardenSSH); applyErr == nil && s != nil && s.Change {
		if err := r.hardenSSH(ctx, admin, adminTarget, deployTarget, o); err != nil {
			applyErr = err
		} else {
			res.Applied = append(res.Applied, StepHardenSSH)
		}
	}
	// Record what was applied, also after a failure part-way, so the
	// packages depper installed are not forgotten.
	if s := plan.step(StepRecord); s != nil && s.Change && (applyErr == nil || len(remoteSteps(res.Applied)) > 0) {
		rec := mergeRecord(facts.Record, recordedOptions(o, plan, res.Applied), remoteSteps(res.Applied), appliedPkgs)
		now := r.now().UTC().Format(time.RFC3339)
		rec.UpdatedAt, rec.DepperVersion = now, r.Version
		if rec.CreatedAt == "" {
			rec.CreatedAt = now
		}
		body, err := r.recordLines(rec)
		if err != nil {
			return err
		}
		done, err := r.applySteps(ctx, admin, []*Step{{ID: StepRecord, lines: body}})
		res.Applied = append(res.Applied, done...)
		if err != nil && applyErr == nil {
			applyErr = fmt.Errorf("writing %s failed: %w", RecordPath, err)
		}
	}
	if applyErr != nil {
		return applyErr
	}

	// 5. Verify as the deploy user with the pinned key, then write the
	// configuration.
	r.printf("Checking the host as %s@%s with depper doctor...\n", o.User, o.Host)
	d := &doctor.Doctor{Config: cfg, Remote: r.remote(cfg.SSHTarget())}
	report := d.Run(ctx)
	res.Doctor = report
	report.WriteHuman(r.out())
	if o.WriteConfig && edit.Changed() {
		if err := edit.apply(cfg.Repo, o.Target, r.LookupEnv); err != nil {
			return err
		}
		res.ConfigWritten = true
		res.Applied = append(res.Applied, StepConfig)
		r.printf("Updated %s [%s].\n", edit.File, edit.Table)
	}
	res.Next = "depper deploy"
	if o.Target != "" {
		res.Next += " --target " + o.Target
	}
	if len(res.Applied) == 0 && res.HostKeyStatus == HostKeyPinned {
		r.printf("Nothing to change: %s is already prepared.\n", o.Host)
	} else {
		r.printf("Host %s is prepared for %s.\n", o.Host, o.User)
	}
	if rel := displayPath(o.KnownHosts, cfg.Repo); !filepath.IsAbs(rel) && !strings.HasPrefix(rel, "~") {
		r.printf("Commit %s (host keys are not secret), or copy its contents into the DEPPER_KNOWN_HOSTS CI secret.\n", rel)
	} else {
		r.printf("Copy %s into the repository or into the DEPPER_KNOWN_HOSTS CI secret so CI trusts the same key.\n", rel)
	}
	if report.Failed() {
		return &DoctorFailedError{}
	}
	r.printf("Next: %s\n", res.Next)
	return nil
}

// DoctorFailedError reports that the final doctor run found failures (they
// were already printed).
type DoctorFailedError struct{}

func (*DoctorFailedError) Error() string {
	return "the host was prepared but depper doctor reports failures (see above)"
}

func (r *Runner) printKeys(keys []HostKey) {
	for _, k := range keys {
		r.printf("  %-20s %s\n", k.Type, k.Fingerprint())
	}
}

func (r *Runner) printPlan(o *Options, f *Facts, p *Plan) {
	name := f.PrettyName
	if name == "" {
		name = f.OSID + " " + f.VersionID
	}
	who := "root"
	if f.Privilege == PrivSudo {
		who = "passwordless sudo"
	}
	r.printf("\n%s: %s (%s), admin %s (%s)\n", o.Host, name, f.Arch, o.Admin, who)
	n := 0
	for _, s := range p.Steps {
		if s.Change {
			n++
			r.printf("  %d. %s: %s\n", n, s.Title, s.Detail)
		}
	}
	if n == 0 {
		r.printf("  Nothing to change.\n")
	}
	for _, s := range p.Steps {
		if !s.Change {
			r.printf("  ok  %s: %s\n", s.Title, s.Detail)
		}
	}
	for _, w := range p.Warnings {
		r.printf("WARNING: %s\n", w)
	}
	r.printf("\n")
}

const (
	beginMarker = "@@depper-begin "
	doneMarker  = "@@depper-done "
)

// markerWriter forwards remote output to the user, line by line, and
// collects the step markers.
type markerWriter struct {
	out  io.Writer
	mu   sync.Mutex
	buf  bytes.Buffer
	done []string
}

func (w *markerWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	for {
		line, err := w.buf.ReadString('\n')
		if err != nil {
			w.buf.Reset()
			w.buf.WriteString(line)
			break
		}
		w.line(strings.TrimRight(line, "\r\n"))
	}
	return len(p), nil
}

func (w *markerWriter) line(l string) {
	switch {
	case strings.HasPrefix(l, doneMarker):
		w.done = append(w.done, strings.TrimPrefix(l, doneMarker))
	case strings.HasPrefix(l, beginMarker):
	default:
		_, _ = fmt.Fprintln(w.out, "  | "+clean(l))
	}
}

func (w *markerWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if rest := w.buf.String(); rest != "" {
		w.line(rest)
		w.buf.Reset()
	}
}

// applySteps runs steps as root in one round trip and returns the IDs that
// completed.
func (r *Runner) applySteps(ctx context.Context, admin *transport.Remote, steps []*Step) ([]string, error) {
	body := []string{"set -eu"}
	for _, s := range steps {
		if s.Title != "" {
			r.printf("==> %s\n", s.Title)
		}
		body = append(body, "echo "+shell.Quote(beginMarker+s.ID))
		body = append(body, s.lines...)
		body = append(body, "echo "+shell.Quote(doneMarker+s.ID))
	}
	lines, err := r.asRoot(body)
	if err != nil {
		return nil, err
	}
	w := &markerWriter{out: r.out()}
	tail := &transport.TailBuffer{Max: 4096}
	rem := *admin
	rem.Stderr = io.MultiWriter(r.out(), tail)
	err = rem.Stream(ctx, shell.Script(lines...), w)
	w.flush()
	if err != nil {
		if msg := lastLines(tail.String(), 3); msg != "" {
			err = fmt.Errorf("%s (%w)", msg, err)
		}
	}
	return w.done, err
}

// runRoot runs a short root script and returns its error.
func (r *Runner) runRoot(ctx context.Context, admin *transport.Remote, body []string) error {
	_, err := r.applySteps(ctx, admin, []*Step{{ID: "root", lines: body}})
	return err
}

const loginMarker = "depper-login-ok"

// verifyLogin opens a NEW ssh connection (no multiplexing) to t and checks
// that it can run a script.
func (r *Runner) verifyLogin(ctx context.Context, t transport.Target) error {
	program := r.SSHProgram
	if program == "" {
		program = "ssh"
	}
	args := append([]string{program}, t.SSHOptions()...)
	args = append(args, "-o", "ControlPath=none", "-T", "--", t.Host, transport.RemoteShell)
	res, err := r.Exec.Run(ctx, transport.Cmd{Args: args, Stdin: strings.NewReader(shell.Wrap("echo " + loginMarker)), Capture: true})
	if err != nil {
		return err
	}
	if !strings.Contains(res.Stdout, loginMarker) {
		return fmt.Errorf("the login did not run the test script")
	}
	return nil
}

// verifyLoginRetry retries verifyLogin while sshd reloads.
func (r *Runner) verifyLoginRetry(ctx context.Context, t transport.Target) error {
	delay := r.retryDelay
	if delay == 0 {
		delay = time.Second
	}
	var err error
	for i := 0; i < 5; i++ {
		if err = r.verifyLogin(ctx, t); err == nil {
			return nil
		}
		if i == 4 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return err
}

// hardenSSH writes the sshd drop-in, validates it, proves the deploy key
// works over a new connection, reloads sshd, and proves new connections
// still work. Any failure removes the drop-in (and reloads when it was
// already loaded).
func (r *Runner) hardenSSH(ctx context.Context, admin *transport.Remote, adminT, deployT transport.Target, o *Options) error {
	r.printf("==> Harden sshd\n")
	dir, file := r.raw(sshdDropInD), r.raw(SSHDropIn)
	write := []string{
		"set -eu",
		"umask 022",
		"mkdir -p " + shell.Quote(dir),
		"printf '%s' " + shell.Quote(dropInContent) + " > " + shell.Quote(file+".depper-tmp"),
		"chmod 644 " + shell.Quote(file+".depper-tmp"),
		"mv -f " + shell.Quote(file+".depper-tmp") + " " + shell.Quote(file),
		`if ! depper_out=$(sshd -t 2>&1); then rm -f ` + shell.Quote(file) + `; printf 'sshd -t rejected the configuration (drop-in removed): %s\n' "$depper_out" >&2; exit 1; fi`,
		`depper_bad=$(sshd -T 2>/dev/null | awk -v admin=` + shell.Quote(o.Admin) + ` '($1 == "passwordauthentication" && $2 != "no") || ($1 == "kbdinteractiveauthentication" && $2 != "no") || ($1 == "permitrootlogin" && $2 != "prohibit-password" && $2 != "without-password" && !($2 == "no" && admin != "root")) { printf "%s %s; ", $1, $2 }')`,
		`if test -n "$depper_bad"; then rm -f ` + shell.Quote(file) + `; printf 'the drop-in does not take effect (sshd -T: %s): an earlier configuration line sets these first; drop-in removed\n' "$depper_bad" >&2; exit 1; fi`,
	}
	remove := []string{"rm -f " + shell.Quote(file)}
	reload := []string{
		`if systemctl is-active --quiet ssh.service; then depper_unit=ssh.service; elif systemctl is-active --quiet sshd.service; then depper_unit=sshd.service; else echo "cannot find the running sshd service (ssh.service or sshd.service)" >&2; exit 1; fi`,
		`systemctl reload "$depper_unit"`,
	}
	if err := r.runRoot(ctx, admin, write); err != nil {
		return fmt.Errorf("--harden-ssh: %w", err)
	}
	r.printf("  checking that %s can log in with its key before reloading sshd...\n", o.User)
	if err := r.verifyLogin(ctx, deployT); err != nil {
		if rerr := r.runRoot(ctx, admin, remove); rerr != nil {
			return fmt.Errorf("--harden-ssh: %s cannot log in with its key (%v), and removing %s failed: %v; remove it by hand before reloading sshd", o.User, err, SSHDropIn, rerr)
		}
		return fmt.Errorf("--harden-ssh: %s cannot log in with its key over a new connection (%v); removed %s and did not reload sshd", o.User, err, SSHDropIn)
	}
	if err := r.runRoot(ctx, admin, reload); err != nil {
		_ = r.runRoot(ctx, admin, remove)
		return fmt.Errorf("--harden-ssh: reloading sshd failed: %w; removed %s", err, SSHDropIn)
	}
	r.printf("  sshd reloaded; checking new connections...\n")
	verr := r.verifyLoginRetry(ctx, deployT)
	who := o.User
	if verr == nil {
		verr = r.verifyLoginRetry(ctx, adminT)
		who = o.Admin
	}
	if verr != nil {
		if rerr := r.runRoot(ctx, admin, append(remove, reload...)); rerr != nil {
			return fmt.Errorf("--harden-ssh: a new login as %s failed after reload (%v), and rolling back failed: %v; remove %s and reload sshd from the provider console", who, verr, rerr, SSHDropIn)
		}
		return fmt.Errorf("--harden-ssh: a new login as %s failed after reload (%v); removed %s and reloaded sshd again", who, verr, SSHDropIn)
	}
	return nil
}

// remoteSteps drops the local steps from a list of step IDs.
func remoteSteps(ids []string) []string {
	var out []string
	for _, id := range ids {
		if id != StepKnownHosts && id != StepConfig {
			out = append(out, id)
		}
	}
	return out
}

// lastLines returns the last n non-empty lines of s, cleaned and joined.
func lastLines(s string, n int) string {
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if l = clean(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "; ")
}

// recordedOptions returns o with every feature whose step was planned but
// did not complete switched off, so the record only claims what is true.
func recordedOptions(o *Options, plan *Plan, applied []string) *Options {
	ro := *o
	ok := func(ids ...string) bool {
		for _, id := range ids {
			if s := plan.step(id); s != nil && s.Change && !contains(applied, id) {
				return false
			}
		}
		return true
	}
	if !ok(StepHardenSSH) {
		ro.HardenSSH = false
	}
	if !ok(StepFirewall) {
		ro.Firewall = nil
	}
	if !ok(StepAutoUpdates) {
		ro.AutoUpdates = false
	}
	if !ok(StepTailscalePkg, StepTailscaleUp) {
		ro.TailscaleKey = Secret{}
	}
	if !ok(StepCloudflared) {
		ro.Cloudflared = false
	}
	if !ok(StepPackages, StepSubIDs, StepDockerGroup, StepDockerService) {
		ro.Container = ContainerNone
	}
	return &ro
}
