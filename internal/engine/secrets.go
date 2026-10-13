package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/frgmt0/depper/internal/dotenv"
	"github.com/frgmt0/depper/internal/shell"
)

// Secrets drift states (EnvDiff.State).
const (
	SecretsInSync  = "in-sync"
	SecretsDrift   = "drift"
	SecretsUnknown = "unknown"
)

// EnvDiff compares a local environment with the host's manifest, by
// salted fingerprint only. JSON keys are stable.
type EnvDiff struct {
	// State is "in-sync", "drift", or "unknown" (no manifest on the host).
	State  string `json:"state"`
	Reason string `json:"reason"`
	// Keys lists variable names by change; never values or fingerprints.
	Keys KeyChanges `json:"keys"`
	// FileModified reports that the live environment file no longer matches
	// the SHA-256 depper recorded (edited outside depper).
	FileModified bool   `json:"file_modified"`
	UpdatedAt    string `json:"updated_at"`
}

// Drift reports a difference that should fail `check` / `secrets diff`.
func (d *EnvDiff) Drift() bool { return d.State == SecretsDrift }

// DiffEnvironment compares values with the host's manifest without writing
// anything (no salt is created).
func (e *Engine) DiffEnvironment(ctx context.Context, l *Layout, values map[string]string) (*EnvDiff, error) {
	empty := KeyChanges{Added: []string{}, Removed: []string{}, Changed: []string{}, Unchanged: []string{}}
	if !l.Deployed() {
		return &EnvDiff{State: SecretsUnknown, Reason: "not deployed", Keys: empty}, nil
	}
	state, err := e.ReadSecretsState(ctx, l)
	if err != nil {
		return nil, err
	}
	return diffFromState(state, values), nil
}

func diffFromState(state *SecretsState, values map[string]string) *EnvDiff {
	if state.Salt == nil || state.Manifest == nil {
		empty := KeyChanges{Added: []string{}, Removed: []string{}, Changed: []string{}, Unchanged: []string{}}
		return &EnvDiff{State: SecretsUnknown, Reason: "no environment manifest on the host yet (written by the next deploy or secrets push)", Keys: empty}
	}
	d := &EnvDiff{
		Keys:         CompareFingerprints(state.Manifest.Keys, Fingerprints(state.Salt, values)),
		FileModified: state.FileModified(),
		UpdatedAt:    sanitizeLabel(state.Manifest.UpdatedAt),
		State:        SecretsInSync,
	}
	var reasons []string
	if !d.Keys.InSync() {
		reasons = append(reasons, "local environment differs from the deployed one")
	}
	if d.FileModified {
		reasons = append(reasons, "the environment file on the host was changed outside depper")
	}
	if len(reasons) > 0 {
		d.State, d.Reason = SecretsDrift, strings.Join(reasons, "; ")
	}
	return d
}

// EnvPushResult summarizes a secrets push. JSON keys are stable.
type EnvPushResult struct {
	Service string        `json:"service"`
	Release string        `json:"release"`
	Env     KeyChanges    `json:"env"`
	Health  *HealthResult `json:"health"`
}

// readCurrent returns the current release id. It fails when the installed
// unit was written by an older depper that read container.env (which this
// version no longer writes): such a deployment needs a deploy first.
func (e *Engine) readCurrent(ctx context.Context, l *Layout) (string, error) {
	out, err := e.Remote.Capture(ctx, shell.Script(
		"set +xv", "set -eu",
		"base="+shell.Quote(l.Base),
		"unit="+unitPathExpr(e.Config.UnitName()),
		`if test -f "$unit" && grep -Fq -e /.depper/container.env "$unit"; then echo "This deployment was made by an older depper that used container.env; run depper deploy once before pushing secrets" >&2; exit 1; fi`,
		`echo @@current`,
		`if test -L "$base/current"; then readlink "$base/current"; fi`,
	))
	if err != nil {
		return "", err
	}
	return releaseFromLink(FromMarker(out, "@@current")), nil
}

// PushEnvironment replaces only the environment (environment file and its
// manifest) under the lock, with a transaction
// backup; it restarts the unit and waits for health, restoring the previous
// environment and restarting again on failure. No release is created. env
// comes from LoadEnvironment.
func (e *Engine) PushEnvironment(ctx context.Context, env *dotenv.Environment) (*EnvPushResult, error) {
	if e.Config.Container != nil {
		if err := CheckContainerEnv(env.Values); err != nil {
			return nil, err
		}
	}
	l, err := e.ProbeForChange(ctx)
	if err != nil {
		return nil, err
	}
	if !l.Deployed() {
		return nil, ErrNotDeployed
	}
	result := &EnvPushResult{Service: e.Config.Service.Name}
	err = e.WithLock(ctx, l, func() error { return e.pushLocked(ctx, l, env, result) })
	return result, err
}

func (e *Engine) pushLocked(ctx context.Context, l *Layout, env *dotenv.Environment, result *EnvPushResult) (err error) {
	current, err := e.readCurrent(ctx, l)
	if err != nil {
		return err
	}
	if current == "" {
		return ErrNotDeployed
	}
	result.Release = current
	artifacts, changes, err := e.prepareEnvironment(ctx, l, env.Values)
	if err != nil {
		return err
	}
	result.Env = changes
	txID, err := e.newTransactionID("secrets")
	if err != nil {
		return err
	}
	defer func() {
		if cerr := e.cleanupCandidates(withoutCancel(ctx), l, txID); cerr != nil && err == nil {
			err = fmt.Errorf("the environment was updated, but removing upload candidates failed: %w", cerr)
		}
	}()
	if err := e.UploadFiles(ctx, envCandidates(l, artifacts, txID)...); err != nil {
		return err
	}
	candidate := func(name string) string { return `"$meta/"` + shell.Quote(name+".next-"+txID) }
	lines := e.txLines(l, txID)
	lines = append(lines, `test -f "$unit" || { echo "The service unit is not installed; run depper deploy first" >&2; exit 1; }`)
	lines = append(lines, backupLines()...)
	lines = append(lines,
		`: > "$tx/`+InterruptedMarker+`"`,
		"mv -Tf "+candidate("environment")+` "$meta/environment"`,
		"mv -Tf "+candidate("environment.manifest")+` "$meta/environment.manifest"`,
	)
	lines = append(lines, "systemctl --user restart "+shell.Quote(e.Config.UnitName()))
	e.printf("Updating the environment of %s (%s)\n", e.Config.Service.Name, changes.Summary())
	health, err := func() (*HealthResult, error) {
		if err := e.Remote.Run(ctx, shell.Script(lines...)); err != nil {
			return nil, err
		}
		return e.WaitHealthy(ctx, l)
	}()
	result.Health = health
	if err != nil {
		cctx := withoutCancel(ctx)
		if rerr := e.Remote.Run(cctx, e.restoreScript(l, txID)); rerr != nil {
			return e.restoreFailedError("environment update", err, rerr, l.Transaction(txID), current)
		}
		e.removeTransaction(cctx, l, txID)
		return fmt.Errorf("environment update failed and the previous environment was restored: %w", err)
	}
	commit := e.txLines(l, txID)
	commit = append(commit, snapshotLines(current)...)
	commit = append(commit, clearInterruptedLines()...)
	commit = append(commit, `rm -rf "$tx"`)
	if err := e.Remote.Run(ctx, shell.Script(commit...)); err != nil {
		return fmt.Errorf("the environment was updated, but recording it for rollback failed: %w", err)
	}
	return nil
}
