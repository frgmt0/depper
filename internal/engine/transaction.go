package engine

import (
	"github.com/frgmt0/depper/internal/shell"
)

// Transactions back up everything an operation replaces into
// <base>/.depper/transactions/<id>/ so a failure can be undone:
//
//	environment, environment.manifest, container.env, unit  (copies)
//	current, previous                                      (symlink targets)
//	no-<name>                                              (absent before)
//
// A successful deploy additionally keeps the configuration it applied in
// transactions/<release>/applied/ (environment, environment.manifest,
// container.env, unit), which `rollback --to` restores. Pruning a release
// deletes its transaction directory, which bounds how long old secret values
// are retained.

// InterruptedMarker is created in a transaction directory when the
// operation starts changing the live configuration, and removed when it is
// committed or restored. A marker that survives means the automatic restore
// failed; `depper rollback` then restores that transaction first.
const InterruptedMarker = "interrupted"

// clearInterruptedLines remove every interrupted marker (after a commit, the
// live state is known-good again).
func clearInterruptedLines() []string {
	return []string{`rm -f "$meta"/transactions/*/` + InterruptedMarker}
}

// txLines starts a transaction script: systemd session variables, a private
// umask, and the variables base, meta, tx, and unit.
func (e *Engine) txLines(l *Layout, txID string) []string {
	lines := append([]string{"set +xv"}, shell.Prelude()...)
	return append(lines,
		"umask 077",
		"base="+shell.Quote(l.Base),
		`meta="$base/.depper"`,
		"tx="+shell.Quote(l.Transaction(txID)),
		"unit="+unitPathExpr(e.Config.UnitName()),
	)
}

// backupLines copy the live configuration into "$tx".
func backupLines() []string {
	return []string{
		`mkdir -p "$tx" "$(dirname "$unit")"`,
		`chmod 700 "$tx"`,
		refuseSymlink(`"$meta/environment"`, "environment file"),
		refuseSymlink(`"$meta/environment.manifest"`, "environment manifest"),
		refuseSymlink(`"$meta/container.env"`, "container environment file"),
		refuseSymlink(`"$unit"`, "unit file"),
		`backup_file() { if test -e "$1"; then cp -p "$1" "$tx/$2"; else : > "$tx/no-$2"; fi; }`,
		`backup_link() { if test -L "$1"; then readlink "$1" > "$tx/$2"; else : > "$tx/no-$2"; fi; }`,
		`backup_file "$meta/environment" environment`,
		`backup_file "$meta/environment.manifest" environment.manifest`,
		`backup_file "$meta/container.env" container.env`,
		`backup_file "$unit" unit`,
		`backup_link "$base/current" current`,
		`backup_link "$base/previous" previous`,
	}
}

// setLinkLines atomically points "$base/<name>" at target (a relative
// releases/<id> path, or a shell expression when raw is set).
func setLinkLines(name, target string, raw bool) []string {
	if !raw {
		target = shell.Quote(target)
	}
	tmp := `"$base/.` + name + `.next"`
	return []string{
		"rm -f " + tmp,
		"ln -s " + target + " " + tmp,
		"mv -Tf " + tmp + ` "$base/` + name + `"`,
	}
}

// restoreScript undoes an operation from transaction txID and restarts the
// unit when one existed before.
func (e *Engine) restoreScript(l *Layout, txID string) string {
	unitName := shell.Quote(e.Config.UnitName())
	lines := e.txLines(l, txID)
	lines = append(lines,
		// Without a backup directory nothing was replaced yet.
		`if ! test -d "$tx"; then exit 0; fi`,
		`restore_file() { if test -f "$tx/$2"; then cp -p "$tx/$2" "$1.restore"; mv -Tf "$1.restore" "$1"; elif test -f "$tx/no-$2"; then rm -f "$1"; fi; }`,
		`restore_link() { if test -f "$tx/$2"; then rm -f "$base/.$2.restore"; ln -s "$(cat "$tx/$2")" "$base/.$2.restore"; mv -Tf "$base/.$2.restore" "$1"; elif test -f "$tx/no-$2"; then rm -f "$1"; fi; }`,
		`restore_file "$meta/environment" environment`,
		`restore_file "$meta/environment.manifest" environment.manifest`,
		`restore_file "$meta/container.env" container.env`,
		`if test -f "$tx/unit"; then cp -p "$tx/unit" "$unit.restore"; mv -Tf "$unit.restore" "$unit"; elif test -f "$tx/no-unit"; then systemctl --user disable --now `+unitName+` || true; rm -f "$unit"; fi`,
		`restore_link "$base/current" current`,
		`restore_link "$base/previous" previous`,
		"systemctl --user daemon-reload",
		`if test -f "$tx/unit"; then systemctl --user restart `+unitName+`; fi`,
		`rm -f "$tx/`+InterruptedMarker+`"`,
	)
	return shell.Script(lines...)
}

// snapshotLines record the live configuration as the applied configuration
// of releaseID (transactions/<releaseID>/applied).
func snapshotLines(releaseID string) []string {
	dir := `"$meta/transactions/"` + shell.Quote(releaseID)
	return []string{
		"mkdir -p " + dir,
		"chmod 700 " + dir,
		"snap=" + dir + "/applied",
		`rm -rf "$snap.next"`,
		`mkdir "$snap.next"`,
		`for name in environment environment.manifest container.env; do if test -f "$meta/$name"; then cp -p "$meta/$name" "$snap.next/$name"; fi; done`,
		`cp -p "$unit" "$snap.next/unit"`,
		`rm -rf "$snap"`,
		`mv -Tf "$snap.next" "$snap"`,
	}
}
