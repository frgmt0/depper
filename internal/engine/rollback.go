package engine

import (
	"context"
	"fmt"
	"sort"

	"github.com/frgmt0/depper/internal/shell"
)

// RollbackResult summarizes a rollback. JSON keys are stable.
type RollbackResult struct {
	Service string        `json:"service"`
	From    string        `json:"from"`
	To      string        `json:"to"`
	Health  *HealthResult `json:"health"`
	// RestoredTransaction is set when `depper rollback` (no --to) restored
	// the saved pre-operation state of an interrupted transaction (a deploy,
	// rollback, or secrets push whose automatic restore failed) instead of
	// switching to the previous release.
	RestoredTransaction string `json:"restored_transaction"`
}

// Rollback switches back to the previous release (or to, when non-empty)
// with the environment and unit that release was deployed with, restarts,
// and waits for health; a failure restores the state from before the
// rollback.
//
// Without to, an interrupted transaction (one whose automatic restore
// failed) takes precedence: the newest one's saved pre-operation state is
// restored instead, since "the previous release" is not meaningful while the
// live state is half-changed.
func (e *Engine) Rollback(ctx context.Context, to string) (*RollbackResult, error) {
	if to != "" && !ValidReleaseID(to) {
		return nil, fmt.Errorf("invalid release id %q", to)
	}
	l, err := e.ProbeForChange(ctx)
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

// newestInterrupted returns the newest interrupted transaction ("" if none).
func newestInterrupted(ids []string) string {
	sorted := append([]string(nil), ids...)
	sort.Slice(sorted, func(i, j int) bool { return idNewer(sorted[i], sorted[j]) })
	if len(sorted) == 0 {
		return ""
	}
	return sorted[0]
}

func (e *Engine) rollbackLocked(ctx context.Context, l *Layout, to string, result *RollbackResult) error {
	list, err := e.ListReleases(ctx, l)
	if err != nil {
		return err
	}
	if to == "" {
		if tx := newestInterrupted(list.interrupted); tx != "" {
			return e.restoreInterrupted(ctx, l, list, tx, result)
		}
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
		`: > "$tx/`+InterruptedMarker+`"`,
		`install_file() { if test -f "$src/$1"; then cp -p "$src/$1" "$2.next"; mv -Tf "$2.next" "$2"; else rm -f "$2"; fi; }`,
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
			return e.restoreFailedError("rollback", err, rerr, l.Transaction(txID), list.Current)
		}
		e.removeTransaction(cctx, l, txID)
		return fmt.Errorf("rollback failed and the previous state was restored: %w", err)
	}
	commit := e.txLines(l, txID)
	commit = append(commit, snapshotLines(target)...)
	commit = append(commit, clearInterruptedLines()...)
	commit = append(commit, `rm -rf "$tx"`)
	if err := e.Remote.Run(ctx, shell.Script(commit...)); err != nil {
		return fmt.Errorf("Rolled back to %s, but recording its configuration failed: %w", target, err)
	}
	return nil
}

// restoreInterrupted restores the pre-operation state saved in the
// interrupted transaction txID, waits for health, and then removes what the
// interrupted operation left behind (for a deploy: its release and image).
func (e *Engine) restoreInterrupted(ctx context.Context, l *Layout, list *ReleaseList, txID string, result *RollbackResult) error {
	if !safeName(txID) {
		return fmt.Errorf("invalid transaction name %q", txID)
	}
	result.From, result.To, result.RestoredTransaction = list.Current, list.interruptedTo[txID], txID
	e.printf("Found the interrupted transaction %s (its automatic restore failed); restoring the state saved before it instead of switching to the previous release.\n", txID)
	if err := e.Remote.Run(ctx, e.restoreScript(l, txID)); err != nil {
		return fmt.Errorf("restoring the state saved in %s failed: %w (the backup is kept; fix the cause and run `depper rollback` again)", l.Transaction(txID), err)
	}
	health, err := e.WaitHealthy(ctx, l)
	result.Health = health
	if err != nil {
		return fmt.Errorf("restored the state saved before the interrupted transaction %s, but the service is not healthy: %w", txID, err)
	}
	lines := e.txLines(l, txID)
	lines = append(lines, clearInterruptedLines()...)
	lines = append(lines, `rm -rf "$tx"`)
	if ValidReleaseID(txID) && txID != result.To {
		// The interrupted deploy's release never went live successfully.
		lines = append(lines, "rm -rf "+shell.Quote(l.Release(txID)))
		if e.usesReleaseImages(l) {
			lines = append(lines, shell.Quote(l.EnginePath)+" rmi "+shell.Quote(e.Config.ImageRef(txID))+" >/dev/null 2>&1 || true")
		}
	}
	if err := e.Remote.Run(ctx, shell.Script(lines...)); err != nil {
		return fmt.Errorf("restored the state saved before %s, but removing its leftovers failed: %w", txID, err)
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
