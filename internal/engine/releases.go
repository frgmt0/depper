package engine

import (
	"context"
	"path"
	"sort"
	"strings"

	"github.com/frgmt0/depper/internal/config"
	"github.com/frgmt0/depper/internal/shell"
)

// ReleaseInfo describes one release directory. JSON keys are stable.
type ReleaseInfo struct {
	ID       string `json:"id"`
	Current  bool   `json:"current"`
	Previous bool   `json:"previous"`
	// RollbackAvailable reports that the release's applied configuration
	// (environment and unit) is still kept, so `rollback --to` can use it.
	RollbackAvailable bool `json:"rollback_available"`
	// Metadata is null for releases without .depper-release.json.
	Metadata *ReleaseMetadata `json:"metadata"`

	// applied reports transactions/<id>/applied exists.
	applied bool
}

// ReleaseList is the state of the releases directory, newest first.
type ReleaseList struct {
	Current  string        `json:"current"`
	Previous string        `json:"previous"`
	Releases []ReleaseInfo `json:"releases"`

	// previousTxRelease is the release whose configuration the
	// previous-transaction backup holds (legacy rollback source).
	previousTxRelease string
	previousTx        string
	transactions      []string
	images            []string
}

// Find returns the release with id.
func (r *ReleaseList) Find(id string) *ReleaseInfo {
	for i := range r.Releases {
		if r.Releases[i].ID == id {
			return &r.Releases[i]
		}
	}
	return nil
}

// releaseFromLink extracts the release id from a current/previous link
// target ("releases/<id>", or an absolute legacy path).
func releaseFromLink(target string) string {
	target = strings.TrimSpace(target)
	if target == "" {
		return ""
	}
	if path.Base(path.Dir(target)) != "releases" {
		return ""
	}
	id := path.Base(target)
	if !ValidReleaseID(id) {
		return ""
	}
	return id
}

func (e *Engine) listScript(l *Layout) string {
	lines := []string{
		"set +xv",
		"set -eu",
		"base=" + shell.Quote(l.Base),
		`meta="$base/.depper"`,
		`echo @@current`,
		`if test -L "$base/current"; then readlink "$base/current"; fi`,
		`echo @@previous`,
		`if test -L "$base/previous"; then readlink "$base/previous"; fi`,
		`echo @@previous-transaction`,
		`ptx=`,
		`if test -f "$meta/previous-transaction"; then ptx=$(head -c 4096 "$meta/previous-transaction"); printf '%s\n' "$ptx"; fi`,
		`echo @@previous-transaction-release`,
		`if test -n "$ptx"; then case "$ptx" in /*) ;; *) ptx="$meta/$ptx" ;; esac; if test -f "$ptx/current" && test -f "$ptx/unit" && test -f "$ptx/environment"; then head -c 4096 "$ptx/current"; echo; fi; fi`,
		`for d in "$base"/releases/*; do`,
		`  test -d "$d" || continue`,
		`  id=${d##*/}`,
		`  printf '@@release %s\n' "$id"`,
		`  if test -f "$meta/transactions/$id/applied/unit" && test -f "$meta/transactions/$id/applied/environment"; then echo applied; else echo none; fi`,
		`  if test -f "$d/` + ReleaseMetadataName + `" && ! test -L "$d/` + ReleaseMetadataName + `"; then head -c 65536 "$d/` + ReleaseMetadataName + `"; echo; fi`,
		`done`,
		`for t in "$meta"/transactions/*; do`,
		`  test -d "$t" || continue`,
		`  printf '@@tx %s\n' "${t##*/}"`,
		`done`,
	}
	if e.Config.Mode() == config.ModeBuild && l.EnginePath != "" {
		lines = append(lines,
			`echo @@images`,
			shell.Quote(l.EnginePath)+` images --format '{{.Repository}}:{{.Tag}}' `+shell.Quote("depper/"+strings.ToLower(e.Config.Service.Name))+` 2>/dev/null || true`,
		)
	}
	return shell.Script(lines...)
}

// ListReleases reads the releases, newest first, with current/previous
// marks and metadata.
func (e *Engine) ListReleases(ctx context.Context, l *Layout) (*ReleaseList, error) {
	if !l.Deployed() {
		return &ReleaseList{Releases: []ReleaseInfo{}}, nil
	}
	out, err := e.Remote.Capture(ctx, e.listScript(l))
	if err != nil {
		return nil, err
	}
	return parseReleaseList(out), nil
}

func parseReleaseList(out string) *ReleaseList {
	list := &ReleaseList{Releases: []ReleaseInfo{}}
	for _, s := range parseSections(out, "") {
		body := strings.TrimSuffix(s.Body, "\n")
		switch {
		case s.Name == "current":
			list.Current = releaseFromLink(body)
		case s.Name == "previous":
			list.Previous = releaseFromLink(body)
		case s.Name == "previous-transaction":
			list.previousTx = strings.TrimSpace(body)
		case s.Name == "previous-transaction-release":
			list.previousTxRelease = releaseFromLink(body)
		case strings.HasPrefix(s.Name, "release "):
			id := strings.TrimPrefix(s.Name, "release ")
			if !ValidReleaseID(id) {
				continue
			}
			marker, rest, _ := strings.Cut(body, "\n")
			info := ReleaseInfo{ID: id, RollbackAvailable: marker == "applied", applied: marker == "applied", Metadata: parseReleaseMetadata(rest)}
			list.Releases = append(list.Releases, info)
		case strings.HasPrefix(s.Name, "tx "):
			list.transactions = append(list.transactions, strings.TrimPrefix(s.Name, "tx "))
		case s.Name == "images":
			for _, line := range strings.Split(body, "\n") {
				if line = strings.TrimSpace(line); line != "" {
					list.images = append(list.images, line)
				}
			}
		}
	}
	sort.Slice(list.Releases, func(i, j int) bool { return list.Releases[i].ID > list.Releases[j].ID })
	for i := range list.Releases {
		r := &list.Releases[i]
		r.Current = r.ID == list.Current
		r.Previous = r.ID == list.Previous
		if !r.RollbackAvailable && r.ID == list.previousTxRelease && r.ID == list.Previous {
			r.RollbackAvailable = true
		}
	}
	return list
}

// pruneSet computes which releases to delete: everything but the newest
// keep releases, current, and previous.
func pruneSet(list *ReleaseList, keep int) (deleteIDs []string, kept map[string]bool) {
	kept = map[string]bool{}
	for i, r := range list.Releases {
		if i < keep || r.ID == list.Current || r.ID == list.Previous {
			kept[r.ID] = true
		} else {
			deleteIDs = append(deleteIDs, r.ID)
		}
	}
	return deleteIDs, kept
}

// Prune deletes releases beyond [deploy].keep_releases (never current or
// previous), their transaction backups (bounding how long old secret values
// are kept), leftover transactions of finished operations, and container
// images tagged for releases that are not kept. Failures are returned as
// warnings: the deploy itself already succeeded.
func (e *Engine) Prune(ctx context.Context, l *Layout) (pruned []string, warnings []string) {
	list, err := e.ListReleases(ctx, l)
	if err != nil {
		return []string{}, []string{"listing releases for pruning failed: " + err.Error()}
	}
	return e.pruneList(ctx, l, list)
}

func (e *Engine) pruneList(ctx context.Context, l *Layout, list *ReleaseList) (pruned []string, warnings []string) {
	pruned = []string{}
	deleteIDs, kept := pruneSet(list, e.Config.Deploy.KeepReleases)
	var rm []string
	for _, id := range deleteIDs {
		rm = append(rm, shell.Quote(l.Release(id)))
	}
	ptxID := path.Base(list.previousTx)
	for _, tx := range list.transactions {
		switch {
		case ValidReleaseID(tx) && kept[tx]:
		case tx == ptxID && list.previousTx != "":
		case ValidReleaseID(tx), strings.HasPrefix(tx, "rollback-"), strings.HasPrefix(tx, "secrets-"):
			if safeName(tx) {
				rm = append(rm, shell.Quote(l.Transaction(tx)))
			}
		}
	}
	lines := []string{"set +xv"}
	lines = append(lines, shell.Prelude()...)
	if len(rm) > 0 {
		lines = append(lines, "rm -rf "+strings.Join(rm, " "))
	}
	var images []string
	if e.Config.Mode() == config.ModeBuild && l.EnginePath != "" {
		repo := "depper/" + strings.ToLower(e.Config.Service.Name)
		for _, ref := range list.images {
			name, tag, ok := strings.Cut(ref, ":")
			if !ok || !(name == repo || strings.HasSuffix(name, "/"+repo)) || !ValidReleaseID(tag) || kept[tag] || shell.HasControl(ref) {
				continue
			}
			images = append(images, ref)
			lines = append(lines, shell.Quote(l.EnginePath)+" rmi "+shell.Quote(ref)+" >/dev/null || echo "+shell.Quote("could not remove image "+ref)+" >&2")
		}
	}
	if len(rm) == 0 && len(images) == 0 {
		return pruned, nil
	}
	if err := e.Remote.Run(ctx, shell.Script(lines...)); err != nil {
		return pruned, []string{"pruning old releases failed: " + err.Error()}
	}
	return append(pruned, deleteIDs...), nil
}

// safeName reports a plain directory entry name.
func safeName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\x00\n\r") && !shell.HasControl(name)
}
