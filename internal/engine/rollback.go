package engine

import (
	"context"
	"fmt"

	"github.com/frgmt0/depper/internal/shell"
)

// RollbackResult summarizes a rollback. JSON keys are stable.
type RollbackResult struct {
	Service string        `json:"service"`
	From    string        `json:"from"`
	To      string        `json:"to"`
	Health  *HealthResult `json:"health"`
}

// Rollback switches back to the previous release (or to, when non-empty)
// with the environment and unit that release was deployed with, restarts,
// and waits for health; a failure restores the state from before the
// rollback.
func (e *Engine) Rollback(ctx context.Context, to string) (*RollbackResult, error) {
	if to != "" && !ValidReleaseID(to) {
		return nil, fmt.Errorf("invalid release id %q", to)
	}
	l, err := e.Probe(ctx, false)
	if err != nil {
		return nil, err
	}
	if !l.Deployed() {
		return nil, ErrNotDeployed
	}
	result := &RollbackResult{Service: e.Config.Service.Name}
	err = e.WithLock(ctx, l, func() error { return e.rollbackLocked(ctx, l, to, result) })
	return result, err
}

func (e *Engine) rollbackLocked(ctx context.Context, l *Layout, to string, result *RollbackResult) error {
	list, err := e.ListReleases(ctx, l)
	if err != nil {
		return err
	}
	target := to
	if target == "" {
		target = list.Previous
		if target == "" {
			return fmt.Errorf("No previous release is available")
		}
	}
	info := list.Find(target)
	if info == nil {
		return fmt.Errorf("release %s no longer exists", target)
	}
	if target == list.Current {
		return fmt.Errorf("release %s is already the current release", target)
	}
	var source string
	switch {
	case info.applied:
		source = l.Transaction(target) + "/applied"
	case target == list.Previous && target == list.previousTxRelease:
		source = resolveTx(l, list.previousTx)
	default:
		return fmt.Errorf("No saved service configuration is available for release %s (its transaction backup was pruned)", target)
	}
	txID, err := e.newTransactionID("rollback")
	if err != nil {
		return err
	}
	result.From, result.To = list.Current, target
	unitName := shell.Quote(e.Config.UnitName())
	lines := e.txLines(l, txID)
	lines = append(lines, backupLines()...)
	lines = append(lines,
		"src="+shell.Quote(source),
		`test -f "$src/unit" && test -f "$src/environment" || { echo "No previous service configuration is available" >&2; exit 1; }`,
		"test -d "+shell.Quote(l.Release(target))+` || { echo "The release no longer exists" >&2; exit 1; }`,
		`install_file() { if test -f "$src/$1"; then cp -p "$src/$1" "$2.next"; mv -f "$2.next" "$2"; else rm -f "$2"; fi; }`,
		`install_file environment "$meta/environment"`,
		`install_file environment.manifest "$meta/environment.manifest"`,
		`install_file container.env "$meta/container.env"`,
		`install_file unit "$unit"`,
	)
	lines = append(lines, setLinkLines("current", "releases/"+target, false)...)
	if list.Current != "" {
		lines = append(lines, setLinkLines("previous", "releases/"+list.Current, false)...)
	}
	lines = append(lines,
		"systemctl --user daemon-reload",
		"systemctl --user restart "+unitName,
	)
	e.printf("Rolling back %s from %s to %s\n", e.Config.Service.Name, orNone(list.Current), target)
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
			return fmt.Errorf("rollback failed: %v; restoring the state before the rollback also failed: %v (backup kept in %s)", err, rerr, l.Transaction(txID))
		}
		e.removeTransaction(cctx, l, txID)
		return fmt.Errorf("rollback failed and the previous state was restored: %w", err)
	}
	commit := e.txLines(l, txID)
	commit = append(commit, snapshotLines(target)...)
	commit = append(commit, `rm -rf "$tx"`)
	if err := e.Remote.Run(ctx, shell.Script(commit...)); err != nil {
		return fmt.Errorf("Rolled back to %s, but recording its configuration failed: %w", target, err)
	}
	return nil
}

func resolveTx(l *Layout, stored string) string {
	if len(stored) > 0 && stored[0] == '/' {
		return stored
	}
	return l.Meta() + "/" + stored
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func (e *Engine) removeTransaction(ctx context.Context, l *Layout, txID string) {
	if err := e.Remote.Run(ctx, shell.Script("set +xv", "set -eu", "rm -rf "+shell.Quote(l.Transaction(txID)))); err != nil {
		e.printf("Warning: removing transaction backup %s failed: %v\n", txID, err)
	}
}
