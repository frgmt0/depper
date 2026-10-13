package engine

import (
	"context"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/frgmt0/depper/internal/config"
	"github.com/frgmt0/depper/internal/shell"
)

// DiscardedMarker is created inside a release whose deploy was interrupted
// before it went live; pruning removes such releases first.
const DiscardedMarker = ".depper-discarded"

// ReleaseInfo describes one release directory. JSON keys are stable.
type ReleaseInfo struct {
	ID       string `json:"id"`
	Current  bool   `json:"current"`
	Previous bool   `json:"previous"`
	// RollbackAvailable reports that the release's applied configuration
	// (environment and unit) is still kept, so `rollback --to` can use it.
	RollbackAvailable bool `json:"rollback_available"`
	// Discarded reports a release left behind by an interrupted deploy; it
	// never went live and is removed by the next prune.
	Discarded bool `json:"discarded"`
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
	// interrupted lists transactions whose interrupted marker survived (a
	// failed automatic restore).
	interrupted []string
	// interruptedTo maps an interrupted transaction to the release its
	// backup restores ("" when none was live before it).
	interruptedTo map[string]string
	images        []string
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

// imageRepo is the repository of the per-release image tags.
func (e *Engine) imageRepo() string {
	return "depper/" + strings.ToLower(e.Config.Service.Name)
}

// usesReleaseImages reports a container mode with per-release image tags.
func (e *Engine) usesReleaseImages(l *Layout) bool {
	mode := e.Config.Mode()
	return (mode == config.ModeBuild || mode == config.ModeImage) && l.EnginePath != ""
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
		`  if test -e "$d/` + DiscardedMarker + `"; then echo discarded; else echo kept; fi`,
		`  if test -f "$d/` + ReleaseMetadataName + `" && ! test -L "$d/` + ReleaseMetadataName + `"; then head -c 65536 "$d/` + ReleaseMetadataName + `"; echo; fi`,
		`done`,
		`for t in "$meta"/transactions/*; do`,
		`  test -d "$t" || continue`,
		`  printf '@@tx %s\n' "${t##*/}"`,
		`  if test -f "$t/` + InterruptedMarker + `"; then echo ` + InterruptedMarker + `; if test -f "$t/current"; then printf 'current=%s\n' "$(head -c 4096 "$t/current")"; fi; fi`,
		`done`,
	}
	if e.usesReleaseImages(l) {
		// Only images labelled for this exact service: another service whose
		// name lowercases to the same repository is never touched.
		lines = append(lines,
			`echo @@images`,
			shell.Quote(l.EnginePath)+` images --filter `+shell.Quote("label="+ImageServiceLabel+"="+e.Config.Service.Name)+` --format '{{.Repository}}:{{.Tag}}' `+shell.Quote(e.imageRepo())+` 2>/dev/null || true`,
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
	list := &ReleaseList{Releases: []ReleaseInfo{}, interruptedTo: map[string]string{}}
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
			discarded := false
			if state, more, ok := strings.Cut(rest+"\n", "\n"); ok && (state == "discarded" || state == "kept") {
				discarded, rest = state == "discarded", strings.TrimSuffix(more, "\n")
			}
			info := ReleaseInfo{ID: id, RollbackAvailable: marker == "applied", applied: marker == "applied", Discarded: discarded, Metadata: parseReleaseMetadata(rest)}
			list.Releases = append(list.Releases, info)
		case strings.HasPrefix(s.Name, "tx "):
			id := strings.TrimPrefix(s.Name, "tx ")
			list.transactions = append(list.transactions, id)
			first, rest, _ := strings.Cut(body, "\n")
			if first == InterruptedMarker && safeName(id) {
				list.interrupted = append(list.interrupted, id)
				list.interruptedTo[id] = releaseFromLink(kv(rest)["current"])
			}
		case s.Name == "images":
			for _, line := range strings.Split(body, "\n") {
				if line = strings.TrimSpace(line); line != "" {
					list.images = append(list.images, line)
				}
			}
		}
	}
	sort.SliceStable(list.Releases, func(i, j int) bool { return releaseNewer(&list.Releases[i], &list.Releases[j]) })
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

// pruneSet computes which releases to delete: discarded releases, and
// everything but the newest keep (non-discarded) releases, current, and
// previous.
func pruneSet(list *ReleaseList, keep int) (deleteIDs []string, kept map[string]bool) {
	kept = map[string]bool{}
	n := 0
	for _, r := range list.Releases {
		switch {
		case r.ID == list.Current || r.ID == list.Previous:
			kept[r.ID] = true
		case r.Discarded:
			deleteIDs = append(deleteIDs, r.ID)
		case n < keep:
			kept[r.ID] = true
		default:
			deleteIDs = append(deleteIDs, r.ID)
		}
		if !r.Discarded {
			n++
		}
	}
	return deleteIDs, kept
}

// Prune deletes releases beyond [deploy].keep_releases (never current or
// previous) and discarded releases, their transaction backups (bounding how
// long old secret values are kept), leftover transactions of finished
// operations, and container images tagged for releases that are not kept.
// Failures are returned as warnings: the deploy itself already succeeded.
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
	if e.usesReleaseImages(l) {
		repo := e.imageRepo()
		engine := shell.Quote(l.EnginePath)
		seen := map[string]bool{}
		// The tags of this service's own pruned releases (release ids are
		// unique, so these are never another service's images); a release
		// without an image (failed build) is not an error...
		for _, id := range deleteIDs {
			seen[id] = true
			ref := e.Config.ImageRef(id)
			images = append(images, ref)
			lines = append(lines, engine+" rmi "+shell.Quote(ref)+" >/dev/null 2>&1 || true")
		}
		// ...and leftovers labelled for this service whose release is gone.
		for _, ref := range list.images {
			name, tag, ok := strings.Cut(ref, ":")
			if !ok || !(name == repo || strings.HasSuffix(name, "/"+repo)) || !ValidReleaseID(tag) || kept[tag] || seen[tag] || list.Find(tag) != nil || shell.HasControl(ref) {
				continue
			}
			seen[tag] = true
			images = append(images, ref)
			lines = append(lines, engine+" rmi "+shell.Quote(ref)+" >/dev/null 2>&1 || echo "+shell.Quote("could not remove image "+ref)+" >&2")
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

// releaseNewer orders releases newest first: by the time in the id
// (milliseconds for current ids), then by the recorded created_at, then by
// id. Second-resolution ids made in the same second therefore fall back to
// metadata rather than to their random suffix alone.
func releaseNewer(a, b *ReleaseInfo) bool {
	ta, tb := idTime(a.ID), idTime(b.ID)
	if !ta.Equal(tb) {
		return ta.After(tb)
	}
	ca, cb := createdAt(a), createdAt(b)
	if !ca.Equal(cb) {
		return ca.After(cb)
	}
	return a.ID > b.ID
}

func createdAt(r *ReleaseInfo) time.Time {
	if r.Metadata == nil {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, r.Metadata.CreatedAt)
	if err != nil {
		return time.Time{}
	}
	return t
}
